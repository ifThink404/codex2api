package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestRelayAuthFailureDoesNotDeleteOrBanAPIAccount(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, AutoCleanUnauthorized: true})
			a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.invalid", APIKey: "test", PlanType: "api", APIAutoRecoveryEnabled: true}
			s.AddAccount(a)
			h := NewHandler(s, nil, nil, nil)
			s.ReportRequestFailure(a, "unauthorized", time.Millisecond)
			h.applyCooldownForModel(a, status, []byte(`{"error":{"code":"auth_unavailable","message":"upstream OAuth failed"}}`), nil, "gpt-test")
			if s.FindByID(1) != a || a.IsBanned() || atomic.LoadInt32(&a.Disabled) != 0 {
				t.Fatal("relay API account deleted or banned by its upstream pool's auth error")
			}
			reason, until := a.GetCooldownSnapshot()
			if reason == "payment_required" || reason == "unauthorized" || time.Until(until) > 5*time.Minute || !until.After(time.Now()) {
				t.Fatalf("expected temporary API upstream backoff, got %s %s", reason, until)
			}
		})
	}
}

func TestResponsesRelayUpstreamAuthFailureKeepsAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"auth_unavailable","type":"server_error","message":"upstream pool auth unavailable"}}`))
			}))
			defer upstream.Close()
			s := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0, AutoCleanUnauthorized: true})
			a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "test", Models: []string{"gpt-test"}, PlanType: "api", APIAutoRecoveryEnabled: true}
			s.AddAccount(a)
			h := NewHandler(s, nil, nil, nil)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test","input":"hello"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			h.Responses(c)
			if calls.Load() == 0 || w.Code < 400 {
				t.Fatalf("test never exercised upstream auth failure: calls=%d status=%d", calls.Load(), w.Code)
			}
			if s.FindByID(1) != a || a.IsBanned() || atomic.LoadInt32(&a.Disabled) != 0 {
				t.Fatal("real Responses path permanently disabled the API account")
			}
			if reason := a.GetCooldownReason(); reason != auth.APIUpstreamUnavailableCooldownReason {
				t.Fatalf("unexpected error handling: %s", reason)
			}
		})
	}
}

func TestAPIRecoveryPreservesExplicitAccountGates(t *testing.T) {
	tests := []struct {
		name, body, reason string
		status             int
		grok               bool
		terminal           bool
	}{
		{"quota on 401", `{"error":{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":3600}}`, "quota", 401, false, false},
		{"quota on 403", `{"error":{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":3600}}`, "quota", 403, false, false},
		{"workspace deactivated", `{"detail":{"code":"deactivated_workspace"}}`, "error", 403, false, true},
		{"runtime deleted", `{"error":{"message":"Agent runtime has been deleted.","code":"biscuit_baker_service_agent_error_status"}}`, "unauthorized", 403, false, true},
		{"Grok quota on 401", `{"error":{"message":"subscription:free-usage-exhausted"}}`, "usage_limited", 401, true, false},
		{"Grok permanent denial", `{"error":{"message":"access to the chat endpoint is denied"}}`, "error", 403, true, true},
		{"billing survives late success", `{"error":{"message":"insufficient balance"}}`, "payment_required", 402, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			t.Cleanup(s.Stop)
			a := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.invalid", APIKey: "test", PlanType: "api", APIAutoRecoveryEnabled: true}
			if tt.grok {
				a.UpstreamType = auth.UpstreamGrok
				a.PlanType = "free"
			}
			s.AddAccount(a)
			h := NewHandler(s, nil, nil, nil)
			decision := h.applyCooldownForModel(a, tt.status, []byte(tt.body), nil, "gpt-test")
			s.ReportRequestSuccess(a, time.Millisecond)
			reason, until := a.GetCooldownSnapshot()
			if reason == auth.APIUpstreamUnavailableCooldownReason {
				t.Fatalf("explicit gate was weakened to temporary recovery: %s", reason)
			}
			if tt.reason == "quota" {
				// Relay model cooldown is independently configurable (off by
				// default). Preserve its existing decision, not an OAuth quota ban.
				if decision.Scope != rateLimitScopeModel || decision.Reason != "rate_limited_model" {
					t.Fatalf("relay quota classification changed: %+v", decision)
				}
				return
			}
			if tt.reason == "error" {
				if a.RuntimeStatus() != "error" {
					t.Fatalf("status=%s want error", a.RuntimeStatus())
				}
			} else if tt.reason != "quota" && reason != tt.reason {
				t.Fatalf("reason=%s want %s", reason, tt.reason)
			}
			if !tt.terminal && !until.After(time.Now().Add(5*time.Minute)) {
				t.Fatalf("stronger gate lost: reason=%s until=%s", reason, until)
			}
			if a.IsAvailable() {
				t.Fatal("explicitly gated account became available after an in-flight success")
			}
		})
	}
}
