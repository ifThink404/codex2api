package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
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
		require.Equal(t, BPSRateLimitedReason, recordBPSFailureClass(ctx, nil, 9001, "gpt-6-astra", http.StatusTooManyRequests, nil, body))
		record, cooling := bpsAccountCooling(ctx, nil, 9001)
		require.True(t, cooling)
		require.Equal(t, BPSRateLimitedReason, record.Reason)
		remaining := time.Until(record.Until)
		require.InDelta(t, want.Seconds(), remaining.Seconds(), 2, "strike %d", i+1)
		require.Greater(t, remaining, previous)
		previous = remaining
	}

	recordBPSFailureClass(ctx, nil, 9002, "", http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}, body)
	record, _ := bpsAccountCooling(ctx, nil, 9002)
	require.InDelta(t, 7, time.Until(record.Until).Seconds(), 2, "Retry-After wins")
	recordBPSFailureClass(ctx, nil, 9003, "", http.StatusTooManyRequests, nil, []byte(`{"error":{"resets_in_seconds":900}}`))
	record, _ = bpsAccountCooling(ctx, nil, 9003)
	require.InDelta(t, 900, time.Until(record.Until).Seconds(), 2, "resets_in_seconds is honored")

	// A strike long after the last one starts over at 60s.
	bpsAccountStateStore.update(ctx, nil, bpsAccountStateKey(9001), time.Now(), func(r *bpsAccountRecord) {
		r.LastStrike, r.Until = time.Now().Add(-time.Hour), time.Time{}
	})
	recordBPSFailureClass(ctx, nil, 9001, "", http.StatusTooManyRequests, nil, body)
	record, _ = bpsAccountCooling(ctx, nil, 9001)
	require.InDelta(t, 60, time.Until(record.Until).Seconds(), 2)

	require.Empty(t, recordBPSFailureClass(ctx, nil, 9004, "", http.StatusInternalServerError, nil, body), "other failures are not tracked")
	_, cooling := bpsAccountCooling(ctx, nil, 9004)
	require.False(t, cooling)
}

func TestBPSAccountStateIsSharedAcrossReplicas(t *testing.T) {
	shared := sharedMemoryCache{cache.NewMemory(1)}
	freshBPSAccountStates(t)
	recordBPSFailureClass(context.Background(), shared, 9011, "", http.StatusTooManyRequests, nil, nil)
	// Another replica: empty local state, same shared cache.
	freshBPSAccountStates(t)
	record, cooling := bpsAccountCooling(context.Background(), shared, 9011)
	require.True(t, cooling, "the cooldown reaches every replica")
	require.Equal(t, BPSRateLimitedReason, record.Reason)
	// Its next strike continues the backoff instead of restarting it.
	recordBPSFailureClass(context.Background(), shared, 9011, "", http.StatusTooManyRequests, nil, nil)
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

	recordBPSFailureClass(context.Background(), nil, nativeToo.ID(), "", http.StatusTooManyRequests, nil, nil)
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

// policyTrigger records threshold blocks, which is one policy trigger.
func policyTrigger(t *testing.T, accountID int64, header http.Header) bpsAccountRecord {
	t.Helper()
	var record bpsAccountRecord
	for range currentBPSConfig().PolicyBlockThreshold {
		_, record = recordBPSFailure(context.Background(), nil, accountID, "", http.StatusForbidden, header, []byte(bpsPolicyBlockBody))
	}
	return record
}

func TestBPSPolicyBlocksClimbTheCooldownLadder(t *testing.T) {
	freshBPSAccountStates(t)
	ctx := context.Background()
	for strike := 1; strike <= 2; strike++ {
		require.Equal(t, BPSPolicyBlockedKind, recordBPSFailureClass(ctx, nil, 9031, "", http.StatusForbidden, nil, []byte(bpsPolicyBlockBody)))
		_, cooling := bpsAccountCooling(ctx, nil, 9031)
		require.False(t, cooling, "strike %d stays below the trigger", strike)
		require.Equal(t, strike, BPSAccountStatuses(ctx, nil, []int64{9031})[0].PolicyStrikes)
	}
	freshBPSAccountStates(t)
	for tier, want := range []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 2 * time.Hour} {
		record := policyTrigger(t, 9032, nil)
		require.Equal(t, BPSPolicyBlockedKind, record.Reason)
		require.Equal(t, min(tier+1, 4), record.PolicyTier)
		require.InDelta(t, want.Seconds(), time.Until(record.Until).Seconds(), 2, "trigger %d", tier+1)
		status := BPSAccountStatuses(ctx, nil, []int64{9032})[0]
		require.Equal(t, min(tier+1, 4), status.PolicyTier)
		require.Equal(t, 4, status.PolicyTiers)
	}

	// An explicit upstream hint is honored exactly, and still climbs a tier.
	record := policyTrigger(t, 9033, http.Header{"X-Ratelimit-Reset-Requests": {"7m30s"}})
	require.InDelta(t, (7*time.Minute + 30*time.Second).Seconds(), time.Until(record.Until).Seconds(), 2)
	require.Equal(t, 1, record.PolicyTier)

	// A tier is forgiven per 2h without a block; everything after 24h.
	for _, tc := range []struct {
		clean      time.Duration
		start, got int
	}{{clean: 3 * time.Hour, start: 3, got: 3}, {clean: 5 * time.Hour, start: 4, got: 3}, {clean: 25 * time.Hour, start: 4, got: 1}} {
		id := int64(9040) + int64(tc.clean/time.Hour)
		bpsAccountStateStore.update(ctx, nil, bpsAccountStateKey(id), time.Now(), func(r *bpsAccountRecord) {
			r.PolicyTier, r.LastPolicyBlock = tc.start, time.Now().Add(-tc.clean)
		})
		require.Equal(t, tc.got, policyTrigger(t, id, nil).PolicyTier, "after %s clean from tier %d", tc.clean, tc.start)
	}

	// Strikes older than the window do not count.
	bpsAccountStateStore.update(ctx, nil, bpsAccountStateKey(9034), time.Now(), func(r *bpsAccountRecord) {
		r.Strikes = []time.Time{time.Now().Add(-11 * time.Minute), time.Now().Add(-12 * time.Minute)}
	})
	recordBPSFailureClass(ctx, nil, 9034, "", http.StatusForbidden, nil, []byte(bpsPolicyBlockBody))
	_, cooling := bpsAccountCooling(ctx, nil, 9034)
	require.False(t, cooling)

	// Trigger size and ladder are configurable.
	updateBPSConfig(t, func(c BPSConfig) BPSConfig {
		c.PolicyBlockThreshold, c.PolicyCooldownLadder = 1, []string{"45s", "5m"}
		return c
	})
	record = policyTrigger(t, 9035, nil)
	require.InDelta(t, 45, time.Until(record.Until).Seconds(), 2)
	require.Equal(t, 2, BPSAccountStatuses(ctx, nil, []int64{9035})[0].PolicyTiers)
	for _, bad := range []string{`{"bps_policy_cooldown_ladder":["soon"]}`, `{"bps_policy_cooldown_ladder":["48h"]}`, `{"bps_policy_cooldown_ladder":["0s"]}`} {
		_, err := parseBPSConfig([]byte(bad))
		require.Error(t, err, bad)
	}
}

func TestBPSFailureHintSources(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		header http.Header
		body   string
		want   time.Duration
	}{
		{name: "none", want: 0},
		{name: "retry-after seconds", header: http.Header{"Retry-After": {"30"}}, want: 30 * time.Second},
		{name: "retry-after-ms", header: http.Header{"Retry-After-Ms": {"1500"}}, want: 1500 * time.Millisecond},
		{name: "x-ratelimit duration", header: http.Header{"X-Ratelimit-Reset-Tokens": {"6m0s"}}, want: 6 * time.Minute},
		{name: "x-ratelimit seconds", header: http.Header{"X-Ratelimit-Reset": {"42"}}, want: 42 * time.Second},
		{name: "x-ratelimit unix", header: http.Header{"X-Ratelimit-Reset": {strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)}}, want: 10 * time.Minute},
		{name: "body resets_in_seconds", body: `{"error":{"resets_in_seconds":90}}`, want: 90 * time.Second},
		{name: "body retry_after", body: `{"retry_after":12}`, want: 12 * time.Second},
		{name: "body resets_at", body: `{"detail":{"error":{"resets_at":` + strconv.FormatInt(now.Add(time.Hour).Unix(), 10) + `}}}`, want: time.Hour},
		{name: "longest wins", header: http.Header{"Retry-After": {"5"}}, body: `{"error":{"resets_in_seconds":60}}`, want: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.InDelta(t, tc.want.Seconds(), bpsFailureHint(tc.header, []byte(tc.body), now).Seconds(), 1)
		})
	}
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
	require.Equal(t, []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour}, parsed.PolicyLadder())
}

const bpsModelAccessBody = `{"error":{"message":"You no longer have access to this model on Basispoints.","type":"invalid_request_error","code":"basispoints_model_access_changed"}}`

func TestBPSModelAccessRefusalRoutesThatModelNatively(t *testing.T) {
	freshBPSAccountStates(t)
	shared := sharedMemoryCache{cache.NewMemory(1)}
	account := withBPSOverride(&auth.Account{DBID: 9051, AccountID: "model-access", AccessToken: "at"}, true)
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.BPSModels = []string{"*"}; return c })
	handler := &Handler{cache: shared}
	req := plugins.NewRequest("req-model", plugins.KindResponses, nil, nil, 0)
	req.SetState(BPSPluginID, &bpsRequest{handler: handler})
	ctx := plugins.WithRequest(context.Background(), req)
	attempt := func(model string) plugins.Attempt {
		return plugins.Attempt{Request: req, Account: account, Model: model}
	}
	require.True(t, bpsPlugin{}.Select(ctx, attempt("gpt-5.5")))

	require.Equal(t, BPSModelUnavailable, bpsFailureClass(http.StatusForbidden, []byte(bpsModelAccessBody)))
	env := &plugins.ReqEnv{Request: req, Account: account, Model: "gpt-5.5"}
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	_, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsModelAccessBody))
	require.NoError(t, err)

	require.False(t, bpsPlugin{}.Select(ctx, attempt("gpt-5.5")), "the refused model is served natively")
	require.True(t, bpsPlugin{}.Select(ctx, attempt("gpt-6-sol")), "other models stay on BPS")
	ok, _ := bpsPlugin{}.Admissible(ctx, account, "gpt-5.5")
	require.True(t, ok, "a model refusal does not take the account out of scheduling")
	_, cooling := bpsAccountCooling(ctx, shared, account.ID())
	require.False(t, cooling)

	statuses := BPSAccountStatuses(ctx, shared, []int64{account.ID()})
	require.InDelta(t, time.Hour.Seconds(), time.Until(statuses[0].ModelsUnavailable["gpt-5.5"]).Seconds(), 5)

	freshBPSAccountStates(t)
	require.True(t, bpsModelBlocked(ctx, shared, account.ID(), "GPT-5.5"), "every replica sees the refusal")
}

func TestBPSModelsConfigDecidesWhichModelsBPSServes(t *testing.T) {
	cfg, err := parseBPSConfig(nil)
	require.NoError(t, err)
	for model, want := range map[string]bool{
		"gpt-6-sol": true, "gpt-5.6-sol": true, "GPT-5.6-luna": true, "codex-auto-review": true,
		"gpt-5.5": false, "gpt-5.3-codex-spark": false, "gpt-6": false,
	} {
		require.Equal(t, want, cfg.ServesModel(model), model)
	}
	require.True(t, cfg.BPSOnlyModel("gpt-6-sol"))
	require.False(t, cfg.BPSOnlyModel("gpt-5.6-sol"))

	custom, err := parseBPSConfig([]byte(`{"bps_models":["*"],"bps_only_models":["gpt-6-sol"]}`))
	require.NoError(t, err)
	require.True(t, custom.ServesModel("gpt-5.5"))
	require.False(t, custom.BPSOnlyModel("gpt-6-luna"))
	for _, bad := range []string{`{"bps_models":["["]}`, `{"bps_models":[""]}`, `{"bps_only_models":["` + strings.Repeat("x", 200) + `"]}`} {
		_, err := parseBPSConfig([]byte(bad))
		require.Error(t, err, bad)
	}

	freshBPSAccountStates(t)
	account := withBPSOverride(&auth.Account{DBID: 9061, AccountID: "models", AccessToken: "at"}, true)
	req := plugins.NewRequest("req-models", plugins.KindResponses, nil, nil, 0)
	req.SetState(BPSPluginID, &bpsRequest{})
	ctx := plugins.WithRequest(context.Background(), req)
	require.False(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: account, Model: "gpt-5.3-codex-spark"}), "models outside bps_models go native")
	require.True(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: account, Model: "gpt-6-sol"}))
}

func TestBPSOnlyModelsPreferBPSCapableAccounts(t *testing.T) {
	freshBPSAccountStates(t)
	bpsAccount := withBPSOverride(&auth.Account{DBID: 9071, AccountID: "bps", AccessToken: "at"}, true)
	nativeAccount := &auth.Account{DBID: 9072, AccountID: "native", AccessToken: "at"}
	req := plugins.NewRequest("req-prefer", plugins.KindResponses, nil, nil, 0)
	req.SetState(BPSPluginID, &bpsRequest{})
	ctx := plugins.WithRequest(context.Background(), req)

	require.Nil(t, bpsPlugin{}.PreferredAccounts(ctx, req, "gpt-5.6-sol"), "native serves gpt-5.6-*: no preference")
	prefer := bpsPlugin{}.PreferredAccounts(ctx, req, "gpt-6-sol")
	require.NotNil(t, prefer)
	require.True(t, prefer(bpsAccount))
	require.False(t, prefer(nativeAccount), "native accounts would reject a BPS-only model")
	recordBPSFailureClass(ctx, nil, bpsAccount.ID(), "", http.StatusTooManyRequests, nil, nil)
	require.False(t, prefer(bpsAccount), "a cooling BPS account is not preferred")
	require.NotNil(t, plugins.Default().PreferenceFilter(ctx, req, "gpt-6-sol"), "the registry exposes the preference")
}

// recordBPSFailureClass is recordBPSFailure returning only the class.
func recordBPSFailureClass(ctx context.Context, store cache.TokenCache, accountID int64, model string, status int, header http.Header, body []byte) string {
	class, _ := recordBPSFailure(ctx, store, accountID, model, status, header, body)
	return class
}

func TestBPSErrorResponseRecordsHeadersAndTierInUsageMeta(t *testing.T) {
	freshBPSAccountStates(t)
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.PolicyBlockThreshold = 1; return c })
	account := withBPSOverride(&auth.Account{DBID: 9081, AccountID: "meta", AccessToken: "at"}, true)
	req := plugins.NewRequest("req-meta", plugins.KindResponses, nil, nil, 0)
	req.SetState(BPSPluginID, &bpsRequest{})
	env := &plugins.ReqEnv{Request: req, Account: account, Model: "gpt-6-sol"}
	bpsPlugin{}.FilterHeaders(env, http.Header{
		"X-Request-Id": {"req_abc"}, "Cf-Ray": {"8f00-LAX"}, "Set-Cookie": {"__cf_bm=secret"}, "X-Ratelimit-Reset-Requests": {"3m"},
	})
	_, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsPolicyBlockBody))
	require.NoError(t, err)
	meta, _ := env.State(bpsAttemptMetaKey).(map[string]string)
	require.Equal(t, "Cf-Ray: 8f00-LAX; Set-Cookie: [REDACTED]; X-Ratelimit-Reset-Requests: 3m; X-Request-Id: req_abc", meta["error_headers"])
	require.Equal(t, BPSPolicyBlockedKind, meta["bps_failure"])
	require.Equal(t, "1", meta["policy_tier"])
	until, err := time.Parse(time.RFC3339, meta["bps_cooling_until"])
	require.NoError(t, err)
	require.InDelta(t, (3 * time.Minute).Seconds(), time.Until(until).Seconds(), 3, "the upstream hint set the cooldown")
}
