package proxy

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/proxy/plugins"
)

func TestNativeHealthAccountSignalClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "revoked token", status: http.StatusUnauthorized, body: `{"error":{"code":"token_revoked"}}`, want: true},
		{name: "invalid token", status: http.StatusUnauthorized, body: `{"error":{"code":"invalid_api_key"}}`, want: true},
		{name: "payment required", status: http.StatusPaymentRequired, body: `{}`, want: true},
		{name: "deactivated workspace", status: http.StatusForbidden, body: `{"detail":{"code":"deactivated_workspace"}}`, want: true},
		{name: "plain forbidden", status: http.StatusForbidden, body: `{"error":{"message":"blocked"}}`},
		{name: "schema refusal", status: http.StatusUnprocessableEntity, body: `{"detail":"invalid input"}`},
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":{"message":"Invalid image"}}`},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`},
		{name: "server error", status: http.StatusInternalServerError, body: `{}`},
		{name: "bad gateway", status: http.StatusBadGateway, body: ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeHealthAccountSignal(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("nativeHealthAccountSignal(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
	for kind, want := range map[string]bool{"unauthorized": true, "server": false, "client": false, "timeout": false, "transport": false, "usage_limit": false} {
		if got := nativeHealthAccountFailureKind(kind); got != want {
			t.Fatalf("nativeHealthAccountFailureKind(%q) = %v, want %v", kind, got, want)
		}
	}
}

type nativeHealthCase struct {
	name, body, event string
	status            int
}

func runNativeHealthCase(t *testing.T, spare bool, tc nativeHealthCase) (streak int, cooled, disabled bool) {
	t.Helper()
	handler, _, plugin, _ := newTransportPluginTestHandler(t, true)
	plugin.spare.Store(spare)
	plugin.failStatus, plugin.failBody, plugin.failEvent = tc.status, tc.body, tc.event
	invokeTracedResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":"hi"}`)
	account := handler.store.FindByID(1)
	account.Mu().RLock()
	streak = account.FailureStreak
	account.Mu().RUnlock()
	return streak, account.HasActiveCooldown(), atomic.LoadInt32(&account.Disabled) == 1
}

func TestTransportPluginSparesNativeHealthForTransportFailures(t *testing.T) {
	for _, tc := range []nativeHealthCase{
		{name: "422", status: http.StatusUnprocessableEntity, body: `{"detail":"invalid input"}`},
		{name: "500", status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`},
		{name: "429", status: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`},
		{name: "stream failure", event: `{"type":"plugin.failed","response":{"id":"resp_p","status":"failed","error":{"code":"server_error","message":"boom"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streak, cooled, disabled := runNativeHealthCase(t, true, tc)
			if streak != 0 || cooled || disabled {
				t.Fatalf("switch on: streak=%d cooled=%v disabled=%v, want untouched account", streak, cooled, disabled)
			}
			// Switch off is today's behavior: the failure reaches native health.
			streak, cooled, _ = runNativeHealthCase(t, false, tc)
			if streak == 0 && !cooled {
				t.Fatalf("switch off: streak=%d cooled=%v, want the failure recorded", streak, cooled)
			}
		})
	}
}

func TestTransportPluginAccountSignalsStillReachNativeHealth(t *testing.T) {
	for _, spare := range []bool{true, false} {
		streak, cooled, disabled := runNativeHealthCase(t, spare, nativeHealthCase{status: http.StatusUnauthorized, body: `{"error":{"code":"token_revoked","message":"revoked"}}`})
		if streak == 0 || !cooled || !disabled {
			t.Fatalf("spare=%v: 401 streak=%d cooled=%v disabled=%v, want the account penalized", spare, streak, cooled, disabled)
		}
	}
}

func TestBPSPluginNativeHealthPolicyFollowsConfig(t *testing.T) {
	var policy plugins.NativeHealthPolicy = bpsPlugin{}
	cfg, err := parseBPSConfig(nil)
	if err != nil || !cfg.SparesNativeHealth() {
		t.Fatalf("default config spares=%v err=%v, want on", cfg.SparesNativeHealth(), err)
	}
	off := false
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.ExcludeFailuresFromNativeHealth = &off; return c })
	if policy.SparesNativeHealth() {
		t.Fatal("exclude_failures_from_native_health=false must report BPS failures")
	}
	parsed, err := parseBPSConfig([]byte(`{"exclude_failures_from_native_health":false}`))
	if err != nil || parsed.SparesNativeHealth() {
		t.Fatalf("parsed false spares=%v err=%v", parsed.SparesNativeHealth(), err)
	}
}

func TestTransportPluginUsageErrorMessageReplacesTheClientMessage(t *testing.T) {
	handler, db, plugin, _ := newTransportPluginTestHandler(t, true)
	plugin.spare.Store(true)
	plugin.failStatus, plugin.failBody = http.StatusBadRequest, `{"error":{"message":"scrubbed for the client"}}`
	plugin.failMessage = "provider original text"
	recorder := invokeTracedResponses(t, handler, `{"model":"gpt-5.5","stream":false,"input":"hi"}`)
	if !strings.Contains(recorder.Body.String(), "scrubbed for the client") || strings.Contains(recorder.Body.String(), "provider original text") {
		t.Fatalf("client body = %s", recorder.Body.String())
	}
	row := onlyUsageLog(t, db)
	if row.ErrorMessage != "provider original text" {
		t.Fatalf("usage error_message = %q, want the provider original", row.ErrorMessage)
	}

	req := plugins.NewRequest("r", plugins.KindResponses, nil, nil, 0)
	req.SetUsageErrorMessage("attempt one")
	if req.UsageErrorMessage() != "attempt one" {
		t.Fatal("message must apply to its own attempt")
	}
}
