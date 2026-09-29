package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

func bpsRoutingHandler(t *testing.T, accounts ...*auth.Account) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-6-sol", MaxRetries: 1})
	t.Cleanup(store.Stop)
	for _, account := range accounts {
		store.AddAccount(account)
	}
	return &Handler{store: store}
}

func bpsRoutingRequest(handler *Handler, body string) (*plugins.Request, *bpsRequest, context.Context) {
	req := plugins.NewRequest("req-routing", plugins.KindResponses, []byte(body), http.Header{}, 7)
	state := &bpsRequest{handler: handler}
	state.conversationKeys = bpsConversationKeys(req, nil)
	state.blocked = state.conversationBlocked(context.Background())
	req.SetState(BPSPluginID, state)
	return req, state, plugins.WithRequest(context.Background(), req)
}

func TestBPSAccountsNeverSpillOntoNativeByDefault(t *testing.T) {
	freshBPSAccountStates(t)
	bpsOnly := withBPSOverride(&auth.Account{DBID: 9101, AccountID: "bps-only", AccessToken: "at"}, true)
	explicitNative := withBPSOverride((&auth.Account{DBID: 9102, AccountID: "explicit-native", AccessToken: "at"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: boolPtrForUpstreamModelTest(true)}), true)
	nativeOnly := &auth.Account{DBID: 9103, AccountID: "native-only", AccessToken: "at"}
	handler := bpsRoutingHandler(t, bpsOnly, explicitNative, nativeOnly)
	req, _, ctx := bpsRoutingRequest(handler, `{"model":"gpt-5.5","input":"hi"}`)

	// gpt-5.5 is not a BPS model: the BPS-only account is vetoed, the account
	// with an explicit native route serves it natively.
	ok, reason := bpsPlugin{}.Admissible(ctx, bpsOnly, "gpt-5.5")
	require.False(t, ok)
	require.Equal(t, BPSModelUnavailable, reason)
	ok, _ = bpsPlugin{}.Admissible(ctx, explicitNative, "gpt-5.5")
	require.True(t, ok)
	require.False(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: explicitNative, Model: "gpt-5.5"}))

	// A cooling BPS-only account is vetoed too; an explicit native one is served natively.
	recordBPSFailureClass(ctx, nil, bpsOnly.ID(), "", http.StatusTooManyRequests, nil, nil)
	recordBPSFailureClass(ctx, nil, explicitNative.ID(), "", http.StatusTooManyRequests, nil, nil)
	ok, reason = bpsPlugin{}.Admissible(ctx, bpsOnly, "gpt-6-sol")
	require.False(t, ok)
	require.Equal(t, BPSRateLimitedReason, reason)
	ok, _ = bpsPlugin{}.Admissible(ctx, explicitNative, "gpt-6-sol")
	require.True(t, ok)
	require.False(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: explicitNative, Model: "gpt-6-sol"}))
}

func TestBPSLastResortReturnsAClearClientError(t *testing.T) {
	freshBPSAccountStates(t)
	only := withBPSOverride(&auth.Account{DBID: 9111, AccountID: "only", AccessToken: "at"}, true)
	handler := bpsRoutingHandler(t, only)
	req, _, ctx := bpsRoutingRequest(handler, `{"model":"gpt-5.5","input":"hi"}`)

	ok, _ := bpsPlugin{}.Admissible(ctx, only, "gpt-5.5")
	require.True(t, ok, "no other account could serve: admit the account so the client gets a clear error")
	require.True(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: only, Model: "gpt-5.5"}), "never spill onto native")
	resp, err := bpsPlugin{}.Execute(ctx, &plugins.ReqEnv{Request: req, Account: only, Model: "gpt-5.5"})
	require.Nil(t, resp)
	var refusal *Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusBadRequest, refusal.HTTPStatus)
	require.Equal(t, "bps_model_unavailable", refusal.Code)
	require.Contains(t, refusal.Message, "model gpt-5.5 is not available on BPS for this account")
}

func TestBPSUsagePolicyHardStop(t *testing.T) {
	freshBPSAccountStates(t)
	blocked := withBPSOverride(&auth.Account{DBID: 9121, AccountID: "blocked", AccessToken: "at"}, true)
	otherBPS := withBPSOverride(&auth.Account{DBID: 9122, AccountID: "other-bps", AccessToken: "at"}, true)
	explicitNative := withBPSOverride((&auth.Account{DBID: 9123, AccountID: "explicit-native", AccessToken: "at"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: boolPtrForUpstreamModelTest(true)}), true)
	handler := bpsRoutingHandler(t, blocked, otherBPS, explicitNative)
	const body = `{"model":"gpt-6-sol","prompt_cache_key":"conversation-a","input":"analyze this code"}`
	req, _, ctx := bpsRoutingRequest(handler, body)

	env := &plugins.ReqEnv{Request: req, Account: blocked, Model: "gpt-6-sol"}
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	_, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsPolicyBlockBody))
	require.NoError(t, err)

	// Never replayed to another BPS account.
	ok, reason := bpsPlugin{}.Admissible(ctx, otherBPS, "gpt-6-sol")
	require.False(t, ok)
	require.Equal(t, BPSPolicyBlockedKind, reason)
	ok, _ = bpsPlugin{}.Admissible(ctx, blocked, "gpt-6-sol")
	require.False(t, ok, "nor retried on the blocked account")
	// Native only through an explicitly enabled native route.
	ok, _ = bpsPlugin{}.Admissible(ctx, explicitNative, "gpt-6-sol")
	require.True(t, ok)
	require.False(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: explicitNative, Model: "gpt-6-sol"}))

	// Follow-up turns of the conversation avoid BPS for 30 minutes.
	_, next, nextCtx := bpsRoutingRequest(handler, `{"model":"gpt-6-sol","prompt_cache_key":"conversation-a","input":"next turn"}`)
	require.True(t, next.policyBlocked())
	ok, reason = bpsPlugin{}.Admissible(nextCtx, otherBPS, "gpt-6-sol")
	require.False(t, ok)
	require.Equal(t, BPSPolicyBlockedKind, reason)
	record := bpsAccountStateStore.load(context.Background(), nil, next.conversationKeys[0], time.Now())
	require.InDelta(t, (30 * time.Minute).Seconds(), time.Until(record.Until).Seconds(), 5)
	// Another conversation, or the same cache key under another API key, is unaffected.
	_, unrelated, _ := bpsRoutingRequest(handler, `{"model":"gpt-6-sol","prompt_cache_key":"conversation-b","input":"hi"}`)
	require.False(t, unrelated.policyBlocked())
	otherKey := plugins.NewRequest("r", plugins.KindResponses, []byte(body), http.Header{}, 8)
	require.NotEqual(t, next.conversationKeys, bpsConversationKeys(otherKey, nil))

	// With no other account able to serve, the client gets the refusal.
	lonely := withBPSOverride(&auth.Account{DBID: 9124, AccountID: "lonely", AccessToken: "at"}, true)
	lonelyReq, lonelyState, lonelyCtx := bpsRoutingRequest(bpsRoutingHandler(t, lonely), body)
	require.True(t, lonelyState.policyBlocked())
	ok, _ = bpsPlugin{}.Admissible(lonelyCtx, lonely, "gpt-6-sol")
	require.True(t, ok)
	_, err = bpsPlugin{}.Execute(lonelyCtx, &plugins.ReqEnv{Request: lonelyReq, Account: lonely, Model: "gpt-6-sol"})
	var refusal *Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusForbidden, refusal.HTTPStatus)
	require.Equal(t, "bps_policy_blocked", refusal.Code)
}
