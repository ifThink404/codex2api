package auth

import (
	"context"
	"errors"
	"testing"
)

type recordingPersister struct {
	calls map[int64]map[string]string
	fail  map[int64]error
}

func (r *recordingPersister) UpdateAccountCustomHeaders(_ context.Context, id int64, headers map[string]string) error {
	if err := r.fail[id]; err != nil {
		return err
	}
	if r.calls == nil {
		r.calls = map[int64]map[string]string{}
	}
	r.calls[id] = headers
	return nil
}

func TestRefreshClaudeFingerprintUserAgent(t *testing.T) {
	old := map[string]string{"user-agent": "claude-cli/2.1.219 (external, cli)", "X-Stainless-OS": "MacOS"}
	next, changed := RefreshClaudeFingerprintUserAgent(old, "2.1.258")
	if !changed || next["user-agent"] != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("should bump version only: %v", next)
	}
	if next["X-Stainless-OS"] != "MacOS" || old["user-agent"] != "claude-cli/2.1.219 (external, cli)" {
		t.Fatal("other headers must be kept and input must not be mutated")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)"}, "2.1.258"); changed {
		t.Fatal("equal version must be a no-op")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "claude-cli/2.1.300 (external, cli)"}, "2.1.258"); changed {
		t.Fatal("newer fingerprint must not be downgraded")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"X-App": "cli"}, "2.1.258"); changed {
		t.Fatal("missing UA must be skipped")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "curl/8.7.1"}, "2.1.258"); changed {
		t.Fatal("non-CLI UA must be skipped")
	}
}

func TestRefreshClaudeFingerprintVersions_PersistsAndAppliesInMemory(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	claudeOld := &Account{DBID: 251, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)", "X-App": "cli"}}
	claudeNew := &Account{DBID: 252, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)"}}
	claudeBroken := &Account{DBID: 253, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.205 (external, cli)"}}
	codex := &Account{DBID: 1, UpstreamType: "codex", CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.100 (external, cli)"}}
	store.mu.Lock()
	store.accounts = []*Account{claudeOld, claudeNew, claudeBroken, codex}
	store.mu.Unlock()

	persister := &recordingPersister{fail: map[int64]error{253: errors.New("db down")}}
	updated, err := RefreshClaudeFingerprintVersions(context.Background(), store, persister, "2.1.258")
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}
	if err == nil || !errors.Is(err, persister.fail[253]) {
		t.Fatalf("first persist error should surface, got %v", err)
	}
	if got := persister.calls[251]["User-Agent"]; got != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("persisted UA = %q", got)
	}
	if persister.calls[251]["X-App"] != "cli" {
		t.Fatal("other fingerprint headers must be persisted unchanged")
	}
	if claudeOld.CustomHeaders["User-Agent"] != "claude-cli/2.1.258 (external, cli)" {
		t.Fatal("in-memory account must be updated after persist")
	}
	if claudeBroken.CustomHeaders["User-Agent"] != "claude-cli/2.1.205 (external, cli)" {
		t.Fatal("failed persist must not update memory")
	}
	if _, called := persister.calls[1]; called {
		t.Fatal("non-Claude accounts must be ignored")
	}
	if _, called := persister.calls[252]; called {
		t.Fatal("up-to-date accounts must not be written")
	}
}

func TestRefreshClaudeFingerprintVersions_RejectsInvalidVersion(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	if _, err := RefreshClaudeFingerprintVersions(context.Background(), store, nil, "nope"); err == nil {
		t.Fatal("invalid target version must error")
	}
}
