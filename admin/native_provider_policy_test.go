package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestProbePolicyNativeExplicitErrorsKeepProviderSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body       string
		status                     int
		terminal, quota, unchanged bool
	}{
		{"deactivated-403", auth.UpstreamOpenAIResponses, `{"error":{"code":"deactivated_workspace"}}`, 403, true, false, false},
		{"deleted-agent-403", auth.UpstreamOpenAIResponses, `{"error":{"code":"biscuit_baker_service_agent_error_status","message":"agent runtime has been deleted"}}`, 403, true, false, false},
		{"usage-limit-401", auth.UpstreamOpenAIResponses, `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`, 401, false, true, false},
		{"usage-limit-429", auth.UpstreamOpenAIResponses, `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`, 429, false, true, false},
		{"grok-free-403", auth.UpstreamGrok, `{"error":{"message":"subscription:free-usage-exhausted tokens (actual/limit): 10/10"}}`, 403, false, true, false},
		{"grok-free-429", auth.UpstreamGrok, `{"error":{"message":"subscription:free-usage-exhausted tokens (actual/limit): 10/10"}}`, 429, false, true, false},
		{"grok-billing-402", auth.UpstreamGrok, `{"error":{"message":"balance_exhausted"}}`, 402, false, true, false},
		{"grok-unknown-402", auth.UpstreamGrok, `{"error":{"message":"unclassified billing response"}}`, 402, false, false, true},
		{"grok-permanent-403", auth.UpstreamGrok, `{"error":{"message":"access to the chat endpoint is denied"}}`, 403, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			s := auth.NewStore(nil, nil, nil)
			defer s.Stop()
			a := &auth.Account{DBID: 1, UpstreamType: tc.provider, APIKey: "key", BaseURL: upstream.URL, Status: auth.StatusReady, PlanType: "free", ProbeMode: "on", APIAutoRecoveryEnabled: true, Models: []string{"gpt-5.6"}}
			if tc.provider == auth.UpstreamGrok {
				a.Models = []string{"grok-4.3"}
			}
			s.AddAccount(a)
			h := &Handler{store: s}
			if err := h.ProbeUsageSnapshot(context.Background(), a); err == nil {
				t.Fatal("provider error reported success")
			}
			reason := a.GetCooldownReason()
			if reason == auth.APIUpstreamUnavailableCooldownReason || reason == auth.ResponsesRateLimitedCooldownReason {
				t.Fatalf("explicit denial became transient: %s", reason)
			}
			if tc.terminal && a.Status != auth.StatusError && !a.IsBanned() {
				t.Fatal("terminal provider constraint bypassed")
			}
			if tc.quota && tc.provider == auth.UpstreamOpenAIResponses {
				// Relay quota follows the existing operator-configured model
				// cooldown (off by default), never OAuth subscription windows.
				if a.IsBanned() || a.UsagePercent5hValid || a.UsagePercent7dValid {
					t.Fatal("relay quota fabricated OAuth state")
				}
				return
			}
			if tc.quota && (a.Status == auth.StatusError || !a.HasActiveCooldown() || !strings.Contains(reason, "usage_limit")) {
				t.Fatalf("quota semantics lost: status=%v reason=%s", a.Status, reason)
			}
			if tc.unchanged && (a.Status != auth.StatusReady || a.IsBanned() || a.HasActiveCooldown()) {
				t.Fatal("unknown Grok billing inferred permanent failure")
			}
			if !a.LastSuccessAt.IsZero() {
				t.Fatal("error recorded success")
			}
		})
	}
}

func TestProbePolicyNativeAntigravityRejectsInvalidEnvelope(t *testing.T) {
	for _, body := range []string{`{}`, `{"hello":"world"}`, `{"id":"i","outputs":[],"error":{"message":"denied"}}`, `{"status":"failed"}`, `{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`} {
		s := auth.NewStore(nil, nil, nil)
		a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamAntigravity, APIKey: "key", ProbeMode: "on", Models: []string{"gemini-3.5-flash-low"}, Status: auth.StatusReady}
		s.AddAccount(a)
		h := &Handler{store: s, antigravityCapabilityProbe: func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}}
		if err := h.ProbeUsageSnapshot(context.Background(), a); err == nil || !a.LastSuccessAt.IsZero() {
			t.Fatalf("invalid AG envelope passed: %s", body)
		}
		s.Stop()
	}
}

func TestProbePolicyDirectCallbackThrottlesFailure(t *testing.T) {
	s := auth.NewStore(nil, nil, nil)
	defer s.Stop()
	a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "key", BaseURL: "https://example.invalid", ProbeMode: "on", ProbeIntervalMinutes: 1, Models: []string{"gpt-5.6"}, Status: auth.StatusReady}
	s.AddAccount(a)
	calls := 0
	h := &Handler{store: s, executeUsageProbe: func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 502, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`bad gateway`))}, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), a); err == nil {
		t.Fatal("first failure was not reported")
	}
	_ = h.ProbeUsageSnapshot(context.Background(), a)
	if calls != 1 || !a.LastSuccessAt.IsZero() {
		t.Fatalf("failed callback throttle calls=%d", calls)
	}
	if a.ProbeInterval(time.Hour) != time.Minute {
		t.Fatal("explicit interval ignored")
	}
}
