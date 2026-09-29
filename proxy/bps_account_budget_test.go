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

// freshBPSBudgets gives a test its own local budget view (a new replica).
func freshBPSBudgets(t *testing.T) {
	t.Helper()
	previousCounter, previousLocal := bpsBudgets, bpsLocalBudgetCache
	bpsBudgets, bpsLocalBudgetCache = &bpsBudgetCounter{}, cache.NewMemory(1)
	t.Cleanup(func() { bpsBudgets, bpsLocalBudgetCache = previousCounter, previousLocal })
}

func TestBPSRequestBudgetCountsAndRollsOver(t *testing.T) {
	freshBPSBudgets(t)
	ctx := context.Background()
	store := cache.NewMemory(1)
	window := 4 * time.Hour
	for range 3 {
		bpsBudgets.record(ctx, store, 9701, window)
	}
	now := time.Now()
	require.Equal(t, 3, readBPSBudget(ctx, bpsGuardedCache(store), 9701, window, now))
	require.Equal(t, 0, readBPSBudget(ctx, bpsGuardedCache(store), 9702, window, now), "per account")
	bucket, _ := bpsBudgetBucket(window, now)
	require.Equal(t, 10*time.Minute, bucket, "24 buckets per window")
	// Still counted just before the window rolls past them, gone after.
	require.Equal(t, 3, readBPSBudget(ctx, bpsGuardedCache(store), 9701, window, now.Add(window-2*bucket)))
	require.Equal(t, 0, readBPSBudget(ctx, bpsGuardedCache(store), 9701, window, now.Add(window+bucket)), "the window rolled")
}

func TestBPSRequestBudgetVetoesAnExhaustedAccount(t *testing.T) {
	freshBPSAccountStates(t)
	freshBPSBudgets(t)
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.AccountRequestBudget, c.AccountBudgetWindow = 2, "1h"; return c })
	account := bpsOrgAccount(9711)
	other := withBPSOverride(&auth.Account{DBID: 9712, AccountID: "budget-other", AccessToken: "at"}, true)
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamComplete)
	}}
	installBPSOrgUpstream(t, account, u)
	for range 2 {
		resp, _, err := runBPSOrgExecute(t, account)
		require.NoError(t, err)
		resp.Body.Close()
	}
	_, _, ctx := bpsRoutingRequest(bpsRoutingHandler(t, account, other), `{"model":"gpt-6-sol","input":"hi"}`)
	ok, reason := bpsPlugin{}.Admissible(ctx, account, "gpt-6-sol")
	require.False(t, ok, "an account at its budget is not used for BPS")
	require.Equal(t, BPSBudgetExhaustedReason, reason)
	ok, _ = bpsPlugin{}.Admissible(ctx, other, "gpt-6-sol")
	require.True(t, ok)
	status := BPSAccountStatuses(ctx, nil, []int64{account.ID(), other.ID()})
	require.Equal(t, 2, status[0].BudgetUsed)
	require.Equal(t, 2, status[0].Budget)
	require.Equal(t, 0, status[1].BudgetUsed)

	// The only account left gets a clear 429.
	req, _, lonelyCtx := bpsRoutingRequest(bpsRoutingHandler(t, account), `{"model":"gpt-6-sol","input":"hi"}`)
	_, err := bpsPlugin{}.Execute(lonelyCtx, &plugins.ReqEnv{Request: req, Account: account, Model: "gpt-6-sol"})
	var refusal *Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusTooManyRequests, refusal.HTTPStatus)
	require.Equal(t, "bps_budget_exhausted", refusal.Code)

	// Failed upstream requests do not count.
	var resp *http.Response
	freshBPSBudgets(t)
	failing := bpsOrgAccount(9713)
	installBPSOrgUpstream(t, failing, &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		return http.StatusInternalServerError, "application/json", `{"error":{"message":"boom"}}`
	}})
	resp, _, err = runBPSOrgExecute(t, failing)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 0, BPSAccountStatuses(context.Background(), nil, []int64{failing.ID()})[0].BudgetUsed)
}

func TestBPSRequestBudgetIsSharedAcrossReplicas(t *testing.T) {
	shared := sharedMemoryCache{cache.NewMemory(1)}
	window := 24 * time.Hour
	freshBPSBudgets(t)
	bpsBudgets.record(context.Background(), shared, 9721, window)
	bpsBudgets.record(context.Background(), shared, 9721, window)
	// Another replica: its own local view, the same shared cache.
	freshBPSBudgets(t)
	require.Equal(t, 2, bpsBudgets.used(context.Background(), shared, 9721, window))
	bpsBudgets.record(context.Background(), shared, 9721, window)
	freshBPSBudgets(t)
	require.Equal(t, 3, bpsBudgets.used(context.Background(), shared, 9721, window), "every replica's requests count")
}

func TestBPSRequestBudgetConfig(t *testing.T) {
	cfg, err := parseBPSConfig(nil)
	require.NoError(t, err)
	require.Zero(t, cfg.AccountRequestBudget, "off by default")
	require.Equal(t, 24*time.Hour, cfg.BudgetWindow())
	custom, err := parseBPSConfig([]byte(`{"bps_account_request_budget":1000,"bps_account_budget_window":"12h"}`))
	require.NoError(t, err)
	require.Equal(t, 12*time.Hour, custom.BudgetWindow())
	for _, bad := range []string{`{"bps_account_request_budget":-1}`, `{"bps_account_budget_window":"30m"}`, `{"bps_account_budget_window":"soon"}`, `{"bps_account_budget_window":"1000h"}`} {
		_, err := parseBPSConfig([]byte(bad))
		require.Error(t, err, bad)
	}
}
