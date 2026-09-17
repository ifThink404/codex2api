package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func v7At(t *testing.T, at time.Time) string {
	t.Helper()
	u, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	ms := uint64(at.UnixMilli())
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	return u.String()
}

func setInitialAdmission(t *testing.T, enabled bool, maxAgeSeconds int) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexInitialSessionAdmissionEnabled = enabled
		s.CodexInitialSessionMaxAgeSeconds = maxAgeSeconds
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

func TestEvaluateInitialSessionAge(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	max := 180 * time.Second
	cases := []struct {
		name string
		id   string
		want initialSessionVerdict
	}{
		{"fresh", v7At(t, now.Add(-10*time.Second)), "allowed"},
		{"exactly max", v7At(t, now.Add(-max)), "allowed"},
		{"expired", v7At(t, now.Add(-max-time.Millisecond)), "expired"},
		{"slightly future within tolerance", v7At(t, now.Add(20*time.Second)), "allowed"},
		{"future beyond tolerance", v7At(t, now.Add(31*time.Second)), "future"},
		{"v4", uuid.NewString(), "invalid"},
		{"garbage", "not-a-uuid", "invalid"},
		{"empty", "", "invalid"},
	}
	for _, tc := range cases {
		if got, _ := evaluateInitialSessionAge(tc.id, now, max); got != tc.want {
			t.Errorf("%s: verdict = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func codexHeaders(sessionID string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", "codex_cli_rs/0.154.0 (Mac OS 26.5.2; arm64) xterm-256color")
	h.Set("Originator", "codex_cli_rs")
	h.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	h.Set("session_id", sessionID)
	return h
}

func TestCheckInitialSessionAdmission(t *testing.T) {
	resetSessionGuardStatsForTest()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	h := &Handler{store: store}
	now := time.Now()
	old := v7At(t, now.Add(-time.Hour))
	fresh := v7At(t, now.Add(-5*time.Second))
	body := []byte(`{"model":"gpt-5.5","input":[]}`)

	setInitialAdmission(t, false, 180)
	if err := h.checkInitialSessionAdmission(codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now); err != nil {
		t.Fatalf("disabled guard must allow: %v", err)
	}

	setInitialAdmission(t, true, 180)
	if err := h.checkInitialSessionAdmission(codexHeaders(fresh), body, resolveRequestSessionIdentity(codexHeaders(fresh), body), false, now); err != nil {
		t.Fatalf("fresh session must be allowed: %v", err)
	}
	err := h.checkInitialSessionAdmission(codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now)
	if err == nil || string(err.Code) != "codex_session_identity_unavailable" {
		t.Fatalf("old unbound session must be rejected, got %v", err)
	}
	details, _ := err.Details.(map[string]any)
	if details == nil || details["retry"] != "stop" {
		t.Fatalf("rejection must carry retry: stop, got %#v", err.Details)
	}
	if err := h.checkInitialSessionAdmission(codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), true, now); err != nil {
		t.Fatalf("bound session must never be age-checked: %v", err)
	}

	plain := http.Header{}
	plain.Set("User-Agent", "curl/8.0")
	plain.Set("X-Session-Id", old)
	if err := h.checkInitialSessionAdmission(plain, body, resolveRequestSessionIdentity(plain, body), false, now); err != nil {
		t.Fatalf("non-Codex clients must be skipped: %v", err)
	}
	v4 := codexHeaders(uuid.NewString())
	if err := h.checkInitialSessionAdmission(v4, body, resolveRequestSessionIdentity(v4, body), false, now); err != nil {
		t.Fatalf("non-v7 ids are counted but never rejected: %v", err)
	}

	_, since := sessionGuardInitialSnapshot(time.Now())
	if since.Samples != 3 || since.Allowed != 1 || since.Expired != 1 || since.Invalid != 1 {
		t.Fatalf("stats = %+v", since)
	}
	if since.MaxAgeMillis < 3_599_000 {
		t.Fatalf("max age must reflect the hour-old sample: %d", since.MaxAgeMillis)
	}
}

func TestEnforceInitialSessionAdmissionSkipsRelayAndMemoizes(t *testing.T) {
	resetSessionGuardStatsForTest()
	setInitialAdmission(t, true, 180)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	h := &Handler{store: store}
	relay := &auth.Account{DBID: 300, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk"}
	if !relay.IsRelayStyle() {
		t.Fatal("test setup: relay account must satisfy IsRelayStyle()")
	}
	official := &auth.Account{DBID: 244, AccessToken: "tok"}
	now := time.Now()
	old := v7At(t, now.Add(-time.Hour))
	body := []byte(`{"model":"gpt-5.5","input":[]}`)
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		return c
	}
	if err := h.enforceInitialSessionAdmission(newCtx(), relay, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now); err != nil {
		t.Fatalf("relay-style account must be exempt: %v", err)
	}
	if _, since := sessionGuardInitialSnapshot(time.Now()); since.Samples != 0 {
		t.Fatalf("relay exemption must not even sample: %+v", since)
	}
	c := newCtx()
	first := h.enforceInitialSessionAdmission(c, official, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now)
	if first == nil {
		t.Fatal("old unbound session on an official account must be rejected")
	}
	second := h.enforceInitialSessionAdmission(c, official, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now)
	if second == nil || second != first {
		t.Fatal("second attempt on the same request must return the memoized verdict")
	}
	if _, since := sessionGuardInitialSnapshot(time.Now()); since.Samples != 1 {
		t.Fatalf("memoized verdict must be sampled once: %+v", since)
	}
}
