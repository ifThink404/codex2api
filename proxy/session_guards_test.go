package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func newSessionGuardTestHandler(t *testing.T, accounts ...*auth.Account) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	for _, account := range accounts {
		store.AddAccount(account)
	}
	return &Handler{store: store}
}

// disableTurnStateVault 关掉 turn-state 托管（默认开）。托管开启时分类只看替身归属、
// 真实 token 不出网关，溯源表/绑定回退那套 legacy 语义测不到，所以测这套的用例必须
// 显式关掉它。
func disableTurnStateVault(t *testing.T) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexTurnStateVaultEnabled = false; return s })
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

// setStrictTurnState 固定 legacy/strict 两态的分类语义：托管开启时 unknown 一律剥离，
// strict 开关就看不出差别，所以一并关掉托管。
func setStrictTurnState(t *testing.T, strict bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexTurnStateStrict = strict
		s.CodexTurnStateVaultEnabled = false
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

func TestApplyCodexTurnStateEchoPolicyClassifiesByExactOrigin(t *testing.T) {
	resetSessionGuardStatsForTest()
	disableTurnStateVault(t)
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	h := newSessionGuardTestHandler(t, minter, other)
	key := "guard-origin::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	noteCodexTurnStateProvenance(key, minter)

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoSame || stripped || headers.Get(codexTurnStateHeader) != "blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "blob" {
		t.Fatalf("same-account echo altered: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}

	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob","thread_id":"t"}}`))
	if class != turnStateEchoCross || !stripped || headers.Get(codexTurnStateHeader) != "" {
		t.Fatalf("cross-account header not stripped: class=%s stripped=%v header=%q", class, stripped, headers.Get(codexTurnStateHeader))
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() || gjson.GetBytes(body, "client_metadata.thread_id").String() != "t" {
		t.Fatalf("cross-account body token must be removed and siblings kept: %s", body)
	}
	totals, accounts := sessionGuardTurnStateSnapshot()
	if totals.Same != 1 || totals.Cross != 1 || totals.Stripped != 1 || len(accounts) != 2 {
		t.Fatalf("counters = %+v accounts=%d", totals, len(accounts))
	}
}

func TestApplyCodexTurnStateEchoPolicyFallsBackToBindingThenUnknown(t *testing.T) {
	resetSessionGuardStatsForTest()
	disableTurnStateVault(t)
	bound := &auth.Account{DBID: 301, AccessToken: "tok"}
	other := &auth.Account{DBID: 302, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, bound, other)
	key := "guard-binding::api-key:9"
	h.store.BindSessionAffinity(key, bound, "")

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, _ := h.applyCodexTurnStateEchoPolicy(key, bound, headers, []byte(`{}`)); class != turnStateEchoSame {
		t.Fatalf("binding-backed same classification = %s", class)
	}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, stripped := h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{}`)); class != turnStateEchoCross || !stripped {
		t.Fatalf("binding-backed cross classification = %s stripped=%v", class, stripped)
	}

	setStrictTurnState(t, false)
	unknownKey := "guard-unknown::api-key:9"
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(unknownKey, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoUnknown || stripped || headers.Get(codexTurnStateHeader) != "blob" || !gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("legacy mode must pass unknown echoes through: class=%s stripped=%v", class, stripped)
	}

	setStrictTurnState(t, true)
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(unknownKey, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoUnknown || !stripped || headers.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("strict mode must strip unknown echoes: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}

	if _, class, _ := h.applyCodexTurnStateEchoPolicy(unknownKey, other, http.Header{}, []byte(`{}`)); class != turnStateEchoNone {
		t.Fatalf("no token must classify as none: %s", class)
	}
	if _, class, _ := h.applyCodexTurnStateEchoPolicy("", other, headers, []byte(`{}`)); class != turnStateEchoNone {
		t.Fatalf("empty affinity key must not be tracked: %s", class)
	}
}

func TestApplyCodexTurnStateEchoPolicyIgnoresExpiredOrigin(t *testing.T) {
	resetSessionGuardStatsForTest()
	setStrictTurnState(t, false)
	other := &auth.Account{DBID: 402}
	h := newSessionGuardTestHandler(t, other)
	key := "guard-expired::api-key:9"
	codexTurnStateOrigins.Store(key, codexTurnStateOrigin{accountID: 401, expiresAt: time.Now().Add(-time.Minute)})
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, _ := h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{}`)); class != turnStateEchoUnknown {
		t.Fatalf("expired origin must fall through to unknown, got %s", class)
	}
}

func TestProjectCodexTurnStateForWebsocket(t *testing.T) {
	setStrictTurnState(t, false)
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	body, out := projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("legacy mode must keep the handshake header: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}

	setStrictTurnState(t, true)
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "blob" {
		t.Fatalf("strict mode must move the token into the frame: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
	if headers.Get(codexTurnStateHeader) != "blob" {
		t.Fatal("caller headers must not be mutated")
	}
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"client_metadata":{"x-codex-turn-state":"frame"}}`), headers)
	if out.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "frame" {
		t.Fatalf("existing frame token must win: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
}
