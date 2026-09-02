package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
)

func withClaudeVersionSources(t *testing.T, github, npm string) {
	t.Helper()
	claudeReleasesLatestURLForTest = github
	claudeNpmDistTagsURLForTest = npm
	t.Cleanup(func() {
		claudeReleasesLatestURLForTest = ""
		claudeNpmDistTagsURLForTest = ""
	})
}

func TestExtractClaudeCLIVersion(t *testing.T) {
	cases := map[string]string{"v2.1.258": "2.1.258", "2.1.258": "2.1.258", " V2.1.259 ": "2.1.259", "2.1.260-beta.1": "2.1.260", "rust-v0.1.0": "", "": "", "2.1": ""}
	for in, want := range cases {
		if got := extractClaudeCLIVersion(in); got != want {
			t.Errorf("extract(%q)=%q want %q", in, got, want)
		}
	}
}

func TestFetchLatestClaudeCLIVersion_PrefersGithub(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.258","tag_name":"v2.1.258"}`))
	}))
	defer gh.Close()
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"latest":"2.1.999"}`))
	}))
	defer npm.Close()
	withClaudeVersionSources(t, gh.URL, npm.URL)
	got, err := FetchLatestClaudeCLIVersion(context.Background(), "")
	if err != nil || got != "2.1.258" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchLatestClaudeCLIVersion_FallsBackToNpm(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer gh.Close()
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"stable":"2.1.236","latest":"2.1.258","next":"2.1.258"}`))
	}))
	defer npm.Close()
	withClaudeVersionSources(t, gh.URL, npm.URL)
	got, err := FetchLatestClaudeCLIVersion(context.Background(), "")
	if err != nil || got != "2.1.258" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchLatestClaudeCLIVersion_BothFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer bad.Close()
	withClaudeVersionSources(t, bad.URL, bad.URL)
	if _, err := FetchLatestClaudeCLIVersion(context.Background(), ""); err == nil {
		t.Fatal("expected error when both sources fail")
	}
}

func TestSyncClaudeCLIVersion_RefreshesFingerprintsWithoutDB(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.300"}`))
	}))
	defer gh.Close()
	withClaudeVersionSources(t, gh.URL, gh.URL)
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	store.SetAccountsForTest([]*auth.Account{{DBID: 251, UpstreamType: auth.UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}})

	result, err := SyncClaudeCLIVersion(context.Background(), nil, store, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.EffectiveVersion != "2.1.300" || result.FetchedVersion != "2.1.300" || result.BuiltinVersion != auth.BuiltinClaudeCLIVersion {
		t.Fatalf("result = %+v", result)
	}
	if result.AccountsRefreshed != 1 {
		t.Fatalf("accounts_refreshed = %d", result.AccountsRefreshed)
	}
	if auth.EffectiveClaudeCLIVersion() != "2.1.300" {
		t.Fatal("runtime effective version must be published")
	}
}
