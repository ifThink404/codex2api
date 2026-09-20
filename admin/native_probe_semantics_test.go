package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestProbePolicyNativeResponsesValidatesTerminalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"malformed", `not-json`, false},
		{"empty", `{}`, false},
		{"wrapped-error", `{"status":"completed","error":{"code":"bad_key"}}`, false},
		{"failed", `{"status":"failed"}`, false},
		{"incomplete-without-reason", `{"status":"incomplete"}`, false},
		{"content-filter", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`, false},
		{"max-tokens", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, true},
		{"stream-max-tokens", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", true},
		{"completed", `{"status":"completed"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := auth.NewStore(nil, nil, nil)
			defer s.Stop()
			a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "key", Models: []string{"gpt-5.6"}, ProbeMode: "on", Status: auth.StatusReady}
			s.AddAccount(a)
			h := &Handler{store: s, executeUsageProbe: func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}}
			err := h.ProbeUsageSnapshot(context.Background(), a)
			if (err == nil) != tc.valid || (!a.LastSuccessAt.IsZero()) != tc.valid {
				t.Fatalf("valid=%v err=%v success=%v", tc.valid, err, a.LastSuccessAt)
			}
		})
	}
}

func TestProbePolicyNativeAPI429IsTransientNotCodexQuota(t *testing.T) {
	s := auth.NewStore(nil, nil, nil)
	defer s.Stop()
	a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "key", Models: []string{"gpt-5.6"}, ProbeMode: "on", Status: auth.StatusReady}
	s.AddAccount(a)
	h := &Handler{store: s, executeUsageProbe: func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"22"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`))}, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), a); err == nil {
		t.Fatal("429 reported success")
	}
	reason, until := a.GetCooldownSnapshot()
	if reason != auth.ResponsesRateLimitedCooldownReason || time.Until(until) > time.Minute || time.Until(until) < 20*time.Second {
		t.Fatalf("429 cooldown=%s until=%v", reason, until)
	}
	if a.UsagePercent5hValid || a.UsagePercent7dValid {
		t.Fatal("API throttle fabricated Codex quota")
	}
}

func TestProbePolicyManualKey403HonorsRecoveryOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(403)
				io.WriteString(w, `{"error":{"code":"access_denied","message":"temporarily forbidden"}}`)
			}))
			defer upstream.Close()
			s := auth.NewStore(nil, nil, nil)
			defer s.Stop()
			a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "key", Models: []string{"gpt-5.6"}, ProbeMode: "off", APIAutoRecoveryEnabled: enabled, Status: auth.StatusReady}
			s.AddAccount(a)
			h := &Handler{store: s}
			status, _ := h.runSingleBatchTest(context.Background(), a)
			if status == "success" {
				t.Fatal("manual 403 succeeded")
			}
			if enabled {
				reason, until := a.GetCooldownSnapshot()
				if reason != auth.APIUpstreamUnavailableCooldownReason || time.Until(until) > 5*time.Minute || a.Status == auth.StatusError {
					t.Fatalf("opt-in lost bounded recovery reason=%s status=%v", reason, a.Status)
				}
			} else if a.Status != auth.StatusError {
				t.Fatalf("opt-out lost prior error behavior: %v", a.Status)
			}
		})
	}
}
