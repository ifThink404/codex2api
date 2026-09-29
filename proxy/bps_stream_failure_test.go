package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSStreamFailureStatus(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{name: "policy block", source: `{"message":"403: This request was blocked by our usage policy.","type":"server_error","code":null}`, want: http.StatusForbidden},
		{name: "model access", source: `{"code":"basispoints_model_access_changed","message":"no access"}`, want: http.StatusForbidden},
		{name: "rate limit code", source: `{"code":"rate_limit_exceeded","message":"slow down"}`, want: http.StatusTooManyRequests},
		{name: "rate limit text", source: `{"message":"429: Rate limit exceeded"}`, want: http.StatusTooManyRequests},
		{name: "usage limit type", source: `{"type":"usage_limit_reached","message":"limit"}`, want: http.StatusTooManyRequests},
		{name: "string source", source: `"This request was blocked by our usage policy."`, want: http.StatusForbidden},
		{name: "server error", source: `{"code":"server_error","message":"boom"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, bpsStreamFailureStatus(gjson.Parse(tc.source)))
		})
	}
}

// streamFailureEnv is an attempt env with a BPS diagnostic, as Execute sets.
func streamFailureEnv(t *testing.T, handler *Handler, account *auth.Account, body string) (*plugins.ReqEnv, *bpsRequest, context.Context) {
	t.Helper()
	req, state, ctx := bpsRoutingRequest(handler, body)
	env := &plugins.ReqEnv{Request: req, Account: account, Model: "gpt-6-sol"}
	env.SetState(bpsAttemptDiagnosticKey, bpsDiagnosticFromContext(bpsProjectionContext(t)))
	bpsPlugin{}.FilterHeaders(env, http.Header{"Content-Type": {"text/event-stream"}})
	return env, state, ctx
}

func TestBPSInStreamPolicyBlockIsTreatedLikeTheHTTPBody(t *testing.T) {
	freshBPSAccountStates(t)
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.PolicyBlockThreshold = 1; return c })
	blocked := withBPSOverride(&auth.Account{DBID: 9401, AccountID: "stream-blocked", AccessToken: "at"}, true)
	other := withBPSOverride(&auth.Account{DBID: 9402, AccountID: "stream-other", AccessToken: "at"}, true)
	// A native-only account can still take the request, so BPS accounts are vetoed.
	handler := bpsRoutingHandler(t, blocked, other, &auth.Account{DBID: 9403, AccountID: "stream-native", AccessToken: "at"})
	env, state, ctx := streamFailureEnv(t, handler, blocked, `{"model":"gpt-6-sol","prompt_cache_key":"stream-conversation","input":"hi"}`)

	frames, err := bpsPlugin{}.TransformSSEFrame(env, "response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"403: This request was blocked by our usage policy.","type":"server_error","code":null}}}`))
	require.NoError(t, err)
	for _, frame := range frames {
		require.NotContains(t, string(frame.Data), "usage policy", "the client still sees the scrubbed error")
	}
	require.Equal(t, BPSPolicyBlockedKind, env.Request.UsageErrorKind(BPSPluginID), "usage kind")
	record, cooling := bpsAccountCooling(ctx, nil, blocked.ID())
	require.True(t, cooling, "policy ladder")
	require.Equal(t, BPSPolicyBlockedKind, record.Reason)
	require.InDelta(t, (2 * time.Minute).Seconds(), time.Until(record.Until).Seconds(), 3)
	require.True(t, state.policyBlocked(), "hard stop")
	ok, reason := bpsPlugin{}.Admissible(ctx, other, "gpt-6-sol")
	require.False(t, ok, "never replayed to another BPS account")
	require.Equal(t, BPSPolicyBlockedKind, reason)
	meta, _ := env.State(bpsAttemptMetaKey).(map[string]string)
	require.Equal(t, "stream", meta["failure_source"])
}

func TestBPSInStreamRateLimitBacksOff(t *testing.T) {
	freshBPSAccountStates(t)
	account := withBPSOverride(&auth.Account{DBID: 9411, AccountID: "stream-429", AccessToken: "at"}, true)
	env, state, ctx := streamFailureEnv(t, bpsRoutingHandler(t, account), account, `{"model":"gpt-6-sol","input":"hi"}`)
	_, err := bpsPlugin{}.TransformSSEFrame(env, "error", []byte(`{"type":"error","code":"rate_limit_exceeded","message":"429: Rate limit exceeded"}`))
	require.NoError(t, err)
	record, cooling := bpsAccountCooling(ctx, nil, account.ID())
	require.True(t, cooling)
	require.Equal(t, BPSRateLimitedReason, record.Reason)
	require.InDelta(t, 60, time.Until(record.Until).Seconds(), 3)
	require.True(t, state.accountExcluded(account), "the request does not retry this account")
	require.False(t, state.policyBlocked())

	// An untracked failure changes nothing.
	freshBPSAccountStates(t)
	env, _, ctx = streamFailureEnv(t, bpsRoutingHandler(t, account), account, `{"model":"gpt-6-sol","input":"hi"}`)
	_, err = bpsPlugin{}.TransformSSEFrame(env, "response.failed", []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`))
	require.NoError(t, err)
	_, cooling = bpsAccountCooling(ctx, nil, account.ID())
	require.False(t, cooling)
}
