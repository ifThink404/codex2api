package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

// policyCooling puts account into a usage-policy cooldown whose tier has
// already expired, waiting for its probe.
func policyCooling(t *testing.T, store cache.TokenCache, accountID int64, tier int) {
	t.Helper()
	bpsAccountStateStore.update(context.Background(), store, bpsAccountStateKey(accountID), time.Now(), func(r *bpsAccountRecord) {
		r.Reason, r.Until, r.NeedsProbe, r.PolicyTier, r.LastPolicyBlock = BPSPolicyBlockedKind, time.Now().Add(-time.Second), true, tier, time.Now().Add(-10*time.Minute)
	})
}

func bpsProbeHandler(t *testing.T, accounts ...*auth.Account) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-6-sol", MaxRetries: 1})
	t.Cleanup(store.Stop)
	for _, account := range accounts {
		store.AddAccount(account)
	}
	return &Handler{store: store}
}

func TestRealRequestsNeverReachAPolicyCoolingAccount(t *testing.T) {
	freshBPSAccountStates(t)
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	cooling := bpsOrgAccount(9801)
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamComplete)
	}}
	installBPSOrgUpstream(t, cooling, u)
	policyCooling(t, nil, cooling.ID(), 3)

	// The tier expired, but no probe succeeded: still vetoed.
	other := withBPSOverride(&auth.Account{DBID: 9802, AccountID: "probe-other", AccessToken: "at"}, true)
	_, _, ctx := bpsRoutingRequest(bpsRoutingHandler(t, cooling, other), `{"model":"gpt-6-sol","input":"hi"}`)
	ok, reason := bpsPlugin{}.Admissible(ctx, cooling, "gpt-6-sol")
	require.False(t, ok)
	require.Equal(t, BPSPolicyBlockedKind, reason)

	// Even as the only account, the real request is refused without upstream.
	req, _, lonelyCtx := bpsRoutingRequest(bpsRoutingHandler(t, cooling), `{"model":"gpt-6-sol","input":"hi"}`)
	_, err := bpsPlugin{}.Execute(lonelyCtx, &plugins.ReqEnv{Request: req, Account: cooling, Model: "gpt-6-sol", Body: []byte(`{"model":"gpt-6-sol","input":"hi"}`)})
	var refusal *Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusServiceUnavailable, refusal.HTTPStatus)
	require.Zero(t, u.calls.Load(), "no real request reached the policy-cooling account")
}

func TestBPSPolicyProbeClearsOrEscalates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		respond     func(int32) (int, string, string)
		result      string
		cleared     bool
		tier        int
		nextProbeIn time.Duration
	}{
		{name: "success clears", result: bpsProbeOK, cleared: true, tier: 2, respond: func(int32) (int, string, string) {
			return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamComplete)
		}},
		{name: "blocked escalates", result: bpsProbeBlocked, tier: 3, respond: func(int32) (int, string, string) {
			return http.StatusForbidden, "application/json", bpsPolicyBlockBody
		}},
		{name: "blocked in stream escalates", result: bpsProbeBlocked, tier: 3, respond: func(int32) (int, string, string) {
			return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, `{"type":"response.failed","response":{"status":"failed","error":{"message":"403: This request was blocked by our usage policy.","type":"server_error","code":null}}}`)
		}},
		{name: "transient error retries later", result: "error: " + BPSRateLimitedReason, tier: 2, nextProbeIn: bpsProbeRetryDelay, respond: func(int32) (int, string, string) {
			return http.StatusTooManyRequests, "application/json", bpsAccountRateBody
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshBPSAccountStates(t)
			t.Setenv("CODEX_TRANSPORT_MODE", "standard")
			account := bpsOrgAccount(9811)
			u := &bpsOrgUpstream{respond: tc.respond}
			installBPSOrgUpstream(t, account, u)
			policyCooling(t, nil, account.ID(), 2)
			h := bpsProbeHandler(t, account)

			h.runBPSPolicyProbes(context.Background(), time.Now())
			require.EqualValues(t, 1, u.calls.Load(), "one synthetic probe")
			record, cooling := bpsAccountCooling(context.Background(), nil, account.ID())
			require.Equal(t, tc.result, record.LastProbeResult)
			require.Equal(t, !tc.cleared, cooling)
			require.Equal(t, tc.tier, record.PolicyTier)
			if tc.result == bpsProbeBlocked {
				require.InDelta(t, (30 * time.Minute).Seconds(), time.Until(record.Until).Seconds(), 5, "the next tier's cooldown")
				require.True(t, record.NeedsProbe)
			}
			if tc.nextProbeIn > 0 {
				require.InDelta(t, tc.nextProbeIn.Seconds(), time.Until(record.NextProbe).Seconds(), 5)
				// Not due again yet: no second probe.
				h.runBPSPolicyProbes(context.Background(), time.Now())
				require.EqualValues(t, 1, u.calls.Load())
			}
			status := BPSAccountStatuses(context.Background(), nil, []int64{account.ID()})[0]
			require.Equal(t, !tc.cleared, status.ProbePending)
			require.Equal(t, tc.result, status.LastProbeResult)
		})
	}
}

func TestBPSPolicyProbeWaitsForTheTierToExpire(t *testing.T) {
	freshBPSAccountStates(t)
	account := bpsOrgAccount(9821)
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) { return http.StatusOK, "text/event-stream", "" }}
	installBPSOrgUpstream(t, account, u)
	bpsAccountStateStore.update(context.Background(), nil, bpsAccountStateKey(account.ID()), time.Now(), func(r *bpsAccountRecord) {
		r.Reason, r.Until, r.NeedsProbe = BPSPolicyBlockedKind, time.Now().Add(time.Hour), true
	})
	bpsProbeHandler(t, account).runBPSPolicyProbes(context.Background(), time.Now())
	require.Zero(t, u.calls.Load(), "no probe while the tier is active")
}

func TestBPSProbeClearingReachesOtherReplicas(t *testing.T) {
	shared := sharedMemoryCache{cache.NewMemory(1)}
	freshBPSAccountStates(t)
	policyCooling(t, shared, 9831, 1)
	// Replica B probes successfully and clears the account.
	freshBPSAccountStates(t)
	bpsAccountStateStore.update(context.Background(), shared, bpsAccountStateKey(9831), time.Now(), func(r *bpsAccountRecord) {
		r.Until, r.Reason, r.NeedsProbe, r.LastProbeResult = time.Time{}, "", false, bpsProbeOK
	})
	// Replica A (fresh view) sees the clear, not the older cooldown.
	freshBPSAccountStates(t)
	_, cooling := bpsAccountCooling(context.Background(), shared, 9831)
	require.False(t, cooling)
	require.Equal(t, bpsProbeOK, bpsAccountStateStore.load(context.Background(), shared, bpsAccountStateKey(9831), time.Now()).LastProbeResult)
}
