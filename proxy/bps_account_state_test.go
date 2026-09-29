package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

// sharedMemoryCache is an in-memory cache that claims to be shared, standing
// in for Redis across simulated replicas.
type sharedMemoryCache struct{ cache.TokenCache }

func (sharedMemoryCache) SharedAcrossInstances() bool { return true }

// freshBPSAccountStates gives a test its own local BPS account state (a new
// replica, as far as local memory goes).
func freshBPSAccountStates(t *testing.T) {
	t.Helper()
	previous := bpsAccountStateStore
	bpsAccountStateStore = &bpsAccountStates{}
	t.Cleanup(func() { bpsAccountStateStore = previous })
}

func TestBPSRateLimitBackoffHonorsHintsAndBacksOffExponentially(t *testing.T) {
	freshBPSAccountStates(t)
	ctx := context.Background()
	body := []byte(`{"error":{"message":"Rate limit exceeded"}}`)
	var previous time.Duration
	for i, want := range []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second} {
		require.Equal(t, BPSRateLimitedReason, recordBPSFailure(ctx, nil, 9001, "gpt-6-astra", http.StatusTooManyRequests, "", body))
		record, cooling := bpsAccountCooling(ctx, nil, 9001)
		require.True(t, cooling)
		require.Equal(t, BPSRateLimitedReason, record.Reason)
		remaining := time.Until(record.Until)
		require.InDelta(t, want.Seconds(), remaining.Seconds(), 2, "strike %d", i+1)
		require.Greater(t, remaining, previous)
		previous = remaining
	}

	recordBPSFailure(ctx, nil, 9002, "", http.StatusTooManyRequests, "7", body)
	record, _ := bpsAccountCooling(ctx, nil, 9002)
	require.InDelta(t, 7, time.Until(record.Until).Seconds(), 2, "Retry-After wins")
	recordBPSFailure(ctx, nil, 9003, "", http.StatusTooManyRequests, "", []byte(`{"error":{"resets_in_seconds":900}}`))
	record, _ = bpsAccountCooling(ctx, nil, 9003)
	require.InDelta(t, 900, time.Until(record.Until).Seconds(), 2, "resets_in_seconds is honored")

	// A strike long after the last one starts over at 60s.
	bpsAccountStateStore.update(ctx, nil, bpsAccountStateKey(9001), time.Now(), func(r *bpsAccountRecord) {
		r.LastStrike, r.Until = time.Now().Add(-time.Hour), time.Time{}
	})
	recordBPSFailure(ctx, nil, 9001, "", http.StatusTooManyRequests, "", body)
	record, _ = bpsAccountCooling(ctx, nil, 9001)
	require.InDelta(t, 60, time.Until(record.Until).Seconds(), 2)

	require.Empty(t, recordBPSFailure(ctx, nil, 9004, "", http.StatusInternalServerError, "", body), "other failures are not tracked")
	_, cooling := bpsAccountCooling(ctx, nil, 9004)
	require.False(t, cooling)
}

func TestBPSAccountStateIsSharedAcrossReplicas(t *testing.T) {
	shared := sharedMemoryCache{cache.NewMemory(1)}
	freshBPSAccountStates(t)
	recordBPSFailure(context.Background(), shared, 9011, "", http.StatusTooManyRequests, "", nil)
	// Another replica: empty local state, same shared cache.
	freshBPSAccountStates(t)
	record, cooling := bpsAccountCooling(context.Background(), shared, 9011)
	require.True(t, cooling, "the cooldown reaches every replica")
	require.Equal(t, BPSRateLimitedReason, record.Reason)
	// Its next strike continues the backoff instead of restarting it.
	recordBPSFailure(context.Background(), shared, 9011, "", http.StatusTooManyRequests, "", nil)
	record, _ = bpsAccountCooling(context.Background(), shared, 9011)
	require.Equal(t, 2, record.Level)
}

func TestBPSPluginRoutesAroundCoolingAccounts(t *testing.T) {
	freshBPSAccountStates(t)
	bpsOnly := withBPSOverride(&auth.Account{DBID: 9021, AccountID: "bps-only", AccessToken: "at"}, true)
	nativeToo := withBPSOverride((&auth.Account{DBID: 9022, AccountID: "native-too", AccessToken: "at"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: boolPtrForUpstreamModelTest(true)}), true)
	req := plugins.NewRequest("req-cooling", plugins.KindResponses, []byte(`{"model":"gpt-6-astra","input":"hi"}`), nil, 0)
	req.SetState(BPSPluginID, &bpsRequest{})
	ctx := plugins.WithRequest(context.Background(), req)

	for _, account := range []*auth.Account{bpsOnly, nativeToo} {
		ok, _ := bpsPlugin{}.Admissible(ctx, account, "gpt-6-astra")
		require.True(t, ok)
	}
	// A BPS 429 on the BPS-only account, seen by the response transformer.
	env := &plugins.ReqEnv{Request: req, Account: bpsOnly, Model: "gpt-6-astra"}
	bpsPlugin{}.FilterHeaders(env, http.Header{"Retry-After": {"120"}})
	_, err := bpsPlugin{}.TransformJSON(env, http.StatusTooManyRequests, []byte(`{"error":{"message":"Rate limit exceeded"}}`))
	require.NoError(t, err)
	ok, reason := bpsPlugin{}.Admissible(ctx, bpsOnly, "gpt-6-astra")
	require.False(t, ok, "a cooling BPS-only account is not scheduled")
	require.Equal(t, "bps_account_refused", reason, "and this request never retries it")

	other := plugins.WithRequest(context.Background(), func() *plugins.Request {
		r := plugins.NewRequest("req-next", plugins.KindResponses, nil, nil, 0)
		r.SetState(BPSPluginID, &bpsRequest{})
		return r
	}())
	ok, reason = bpsPlugin{}.Admissible(other, bpsOnly, "gpt-6-astra")
	require.False(t, ok)
	require.Equal(t, BPSRateLimitedReason, reason, "later requests skip it until the cooldown ends")

	recordBPSFailure(context.Background(), nil, nativeToo.ID(), "", http.StatusTooManyRequests, "", nil)
	ok, _ = bpsPlugin{}.Admissible(other, nativeToo, "gpt-6-astra")
	require.True(t, ok, "an account native can serve stays schedulable")
	require.False(t, bpsPlugin{}.Select(other, plugins.Attempt{Request: plugins.RequestFromContext(other), Account: nativeToo, Model: "gpt-6-astra"}), "and is served natively while BPS cools")

	statuses := BPSAccountStatuses(context.Background(), nil, []int64{bpsOnly.ID(), nativeToo.ID(), 9099})
	require.Len(t, statuses, 3)
	require.Equal(t, BPSRateLimitedReason, statuses[0].Reason)
	require.InDelta(t, 120, time.Until(statuses[0].CoolingUntil).Seconds(), 2)
	require.True(t, statuses[2].CoolingUntil.IsZero())
}

const bpsPolicyBlockBody = `{"error":{"message":"This request was blocked by our usage policy.","type":"server_error","param":null,"code":null}}`

func TestBPSFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "usage policy block", status: http.StatusForbidden, body: bpsPolicyBlockBody, want: BPSPolicyBlockedKind},
		{name: "usage policy block in detail", status: http.StatusForbidden, body: `{"detail":"This request was blocked by our usage policy."}`, want: BPSPolicyBlockedKind},
		{name: "other forbidden", status: http.StatusForbidden, body: `{"error":{"message":"Forbidden"}}`},
		{name: "policy text on another status", status: http.StatusBadRequest, body: bpsPolicyBlockBody},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"error":{"message":"Rate limit exceeded"}}`, want: BPSRateLimitedReason},
		{name: "server error", status: http.StatusInternalServerError, body: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, bpsFailureClass(tc.status, []byte(tc.body)))
		})
	}
}

func TestBPSPolicyBlocksStopBPSRoutingAfterThreshold(t *testing.T) {
	freshBPSAccountStates(t)
	ctx := context.Background()
	for strike := 1; strike <= 2; strike++ {
		require.Equal(t, BPSPolicyBlockedKind, recordBPSFailure(ctx, nil, 9031, "", http.StatusForbidden, "", []byte(bpsPolicyBlockBody)))
		_, cooling := bpsAccountCooling(ctx, nil, 9031)
		require.False(t, cooling, "strike %d stays below the threshold", strike)
		require.Equal(t, strike, BPSAccountStatuses(ctx, nil, []int64{9031})[0].PolicyStrikes)
	}
	recordBPSFailure(ctx, nil, 9031, "", http.StatusForbidden, "", []byte(bpsPolicyBlockBody))
	record, cooling := bpsAccountCooling(ctx, nil, 9031)
	require.True(t, cooling)
	require.Equal(t, BPSPolicyBlockedKind, record.Reason)
	require.InDelta(t, (6 * time.Hour).Seconds(), time.Until(record.Until).Seconds(), 5, "default long cooldown")
	require.Zero(t, BPSAccountStatuses(ctx, nil, []int64{9031})[0].PolicyStrikes)

	// Strikes older than the window do not count.
	bpsAccountStateStore.update(ctx, nil, bpsAccountStateKey(9032), time.Now(), func(r *bpsAccountRecord) {
		r.Strikes = []time.Time{time.Now().Add(-11 * time.Minute), time.Now().Add(-12 * time.Minute)}
	})
	recordBPSFailure(ctx, nil, 9032, "", http.StatusForbidden, "", []byte(bpsPolicyBlockBody))
	_, cooling = bpsAccountCooling(ctx, nil, 9032)
	require.False(t, cooling)

	// Threshold and cooldown are configurable.
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.PolicyBlockThreshold, c.PolicyBlockCooldownHours = 1, 2; return c })
	recordBPSFailure(ctx, nil, 9033, "", http.StatusForbidden, "", []byte(bpsPolicyBlockBody))
	record, cooling = bpsAccountCooling(ctx, nil, 9033)
	require.True(t, cooling)
	require.InDelta(t, (2 * time.Hour).Seconds(), time.Until(record.Until).Seconds(), 5)
}

func TestBPSPluginPolicyBlockNeverRetriesTheSameAccount(t *testing.T) {
	freshBPSAccountStates(t)
	account := withBPSOverride(&auth.Account{DBID: 9041, AccountID: "policy", AccessToken: "at"}, true)
	req := plugins.NewRequest("req-policy", plugins.KindResponses, nil, nil, 0)
	req.SetState(BPSPluginID, &bpsRequest{})
	ctx := plugins.WithRequest(context.Background(), req)
	env := &plugins.ReqEnv{Request: req, Account: account, Model: "gpt-6-astra"}
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	out, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsPolicyBlockBody))
	require.NoError(t, err)
	require.NotContains(t, string(out), "usage policy", "the client still gets the scrubbed error")
	require.Equal(t, BPSPolicyBlockedKind, req.UsageErrorKind(BPSPluginID), "the usage row records the classification")
	require.Contains(t, req.UsageErrorMessage(), "blocked by our usage policy", "and the provider's own text")
	ok, reason := bpsPlugin{}.Admissible(ctx, account, "gpt-6-astra")
	require.False(t, ok)
	require.Equal(t, "bps_account_refused", reason)

	parsed, err := parseBPSConfig(nil)
	require.NoError(t, err)
	require.Equal(t, 3, parsed.PolicyBlockThreshold)
	require.Equal(t, 6, parsed.PolicyBlockCooldownHours)
}
