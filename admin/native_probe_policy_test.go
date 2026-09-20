package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestProbePolicyNativeAPIValidationNeverCallsWham(t *testing.T) {
	for _, provider := range []string{auth.UpstreamOpenAIResponses, auth.UpstreamClaude, auth.UpstreamGrok, auth.UpstreamAntigravity} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.Contains(r.URL.Path, "wham") || strings.Contains(r.URL.Path, "usage") {
					t.Errorf("API credential sent to quota endpoint %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n")
			}))
			defer upstream.Close()
			s := auth.NewStore(nil, nil, nil)
			defer s.Stop()
			a := &auth.Account{DBID: 1, UpstreamType: provider, APIKey: "key", BaseURL: upstream.URL, Status: auth.StatusReady, ProbeMode: "on", ProbeIntervalMinutes: 1, Models: []string{"gpt-5.6"}}
			h := &Handler{store: s}
			if provider == auth.UpstreamClaude {
				a.APIKey = ""
				a.AccessToken = "claude-key"
				a.ClaudeAuthKind = auth.ClaudeAuthKindAPIKey
				a.ClaudeBaseURL = upstream.URL
				a.Models = []string{"claude-haiku-4-5"}
				h.executeClaudeUsageProbe = func(context.Context, *auth.Account, []byte) (*http.Response, error) {
					calls.Add(1)
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"type":"message","content":[{"type":"text","text":"OK"}]}`))}, nil
				}
			}
			if provider == auth.UpstreamGrok {
				a.Models = []string{"grok-4.3"}
			}
			if provider == auth.UpstreamAntigravity {
				a.Models = []string{"gemini-3.5-flash-low"}
				h.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
					calls.Add(1)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"interaction-1","outputs":[]}`))}, nil
				}
			}
			s.AddAccount(a)
			if err := h.ProbeUsageSnapshot(context.Background(), a); err != nil {
				t.Fatalf("native validation: %v", err)
			}
			if calls.Load() != 1 || a.LastSuccessAt.IsZero() {
				t.Fatalf("native validation attempts=%d success=%v, want 1 and recorded success", calls.Load(), a.LastSuccessAt)
			}
		})
	}
}

func TestProbePolicyDirectCallbackOffAndAutoKeysDoNotProbe(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		s := auth.NewStore(nil, nil, nil)
		a := &auth.Account{UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "key", ProbeMode: mode}
		calls := 0
		h := &Handler{store: s, executeClaudeUsageProbe: func(context.Context, *auth.Account, []byte) (*http.Response, error) { calls++; return nil, nil }}
		if err := h.ProbeUsageSnapshot(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatalf("%s made %d requests", mode, calls)
		}
		s.Stop()
	}
}

func TestProbePolicyNativeSuccessPreservesPauseAndActiveFailure(t *testing.T) {
	s := auth.NewStore(nil, nil, nil)
	defer s.Stop()
	a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "key", BaseURL: "https://example.test", Status: auth.StatusReady, ProbeMode: "on", APIAutoRecoveryEnabled: true, Models: []string{"gpt-5.6"}}
	s.AddAccount(a)
	h := &Handler{store: s, executeUsageProbe: func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
		s.MarkAPIUpstreamUnavailable(a, time.Minute, "newer failure")
		atomic.StoreInt32(&a.DispatchPaused, 1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 1 || !a.HasActiveCooldown() {
		t.Fatal("background success cleared manual pause or newer failure")
	}
}
