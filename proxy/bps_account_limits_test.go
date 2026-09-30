package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

func freshBPSInflight(t *testing.T) {
	t.Helper()
	previous := bpsInflightRequests
	bpsInflightRequests = &bpsInflight{}
	t.Cleanup(func() { bpsInflightRequests = previous })
}

func TestBPSConcurrencyCapHoldsUnderLoad(t *testing.T) {
	freshBPSAccountStates(t)
	freshBPSInflight(t)
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.AccountMaxConcurrency = 2; return c })
	account := bpsOrgAccount(9601)
	var inflight, peak atomic.Int32
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		now := inflight.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inflight.Add(-1)
		return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamComplete)
	}}
	installBPSOrgUpstream(t, account, u)
	var wg sync.WaitGroup
	var failures atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _, err := runBPSOrgExecute(t, account)
			if err != nil {
				failures.Add(1)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	require.Zero(t, failures.Load(), "waiting requests get a slot")
	require.EqualValues(t, 8, u.calls.Load())
	require.LessOrEqual(t, peak.Load(), int32(2), "never more than the cap in flight")
	require.Zero(t, bpsInflightRequests.current(account.ID()), "every slot is released")
}

func TestBPSConcurrencyCapRoutesAroundFullAccounts(t *testing.T) {
	freshBPSAccountStates(t)
	freshBPSInflight(t)
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.AccountMaxConcurrency = 1; return c })
	full := withBPSOverride(&auth.Account{DBID: 9611, AccountID: "full", AccessToken: "at"}, true)
	free := withBPSOverride(&auth.Account{DBID: 9612, AccountID: "free", AccessToken: "at"}, true)
	release, ok := bpsInflightRequests.tryAcquire(full.ID(), 1)
	require.True(t, ok)
	defer release()

	_, _, ctx := bpsRoutingRequest(bpsRoutingHandler(t, full, free), `{"model":"gpt-6-sol","input":"hi"}`)
	ok, reason := bpsPlugin{}.Admissible(ctx, full, "gpt-6-sol")
	require.False(t, ok, "a full account is skipped while another BPS account is free")
	require.Equal(t, BPSConcurrencyFullReason, reason)
	ok, _ = bpsPlugin{}.Admissible(ctx, free, "gpt-6-sol")
	require.True(t, ok)
	require.Equal(t, 1, BPSAccountStatuses(ctx, nil, []int64{full.ID()})[0].InFlight)

	// The only account: admitted, waits briefly for a slot, then a clear 429.
	previous := bpsConcurrencyWait
	bpsConcurrencyWait = 30 * time.Millisecond
	t.Cleanup(func() { bpsConcurrencyWait = previous })
	req, _, lonelyCtx := bpsRoutingRequest(bpsRoutingHandler(t, full), `{"model":"gpt-6-sol","input":"hi"}`)
	ok, _ = bpsPlugin{}.Admissible(lonelyCtx, full, "gpt-6-sol")
	require.True(t, ok)
	_, err := bpsPlugin{}.Execute(lonelyCtx, &plugins.ReqEnv{Request: req, Account: full, Model: "gpt-6-sol"})
	var refusal *Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusTooManyRequests, refusal.HTTPStatus)
	require.Equal(t, "bps_concurrency_limited", refusal.Code)
}

func TestBPSConcurrencyCapOffByDefault(t *testing.T) {
	cfg, err := parseBPSConfig(nil)
	require.NoError(t, err)
	require.Zero(t, cfg.AccountMaxConcurrency)
	require.False(t, bpsConcurrencyFull(&auth.Account{DBID: 1}))
	for _, bad := range []string{`{"bps_account_max_concurrency":-1}`, `{"bps_account_max_concurrency":101}`} {
		_, err := parseBPSConfig([]byte(bad))
		require.Error(t, err, bad)
	}
	body := &bpsReleasingBody{ReadCloser: io.NopCloser(strings.NewReader("x")), release: func() {}}
	_, _ = io.ReadAll(body)
	require.NoError(t, body.Close())
}

func TestBPSActivityIsTrackedWithoutLimits(t *testing.T) {
	freshBPSAccountStates(t)
	freshBPSInflight(t)
	freshBPSBudgets(t)
	account := bpsOrgAccount(9651)
	status := http.StatusOK
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		if status != http.StatusOK {
			return status, "application/json", `{"error":{"message":"boom"}}`
		}
		return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamComplete)
	}}
	installBPSOrgUpstream(t, account, u)
	started := time.Now()
	resp, _, err := runBPSOrgExecute(t, account)
	require.NoError(t, err)
	require.Equal(t, 1, bpsInflightRequests.current(account.ID()), "in flight while the response is open, with no cap set")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.Zero(t, bpsInflightRequests.current(account.ID()))
	require.False(t, bpsLastRequestAt(account.ID()).Before(started), "last request time recorded")

	status = http.StatusInternalServerError
	resp, _, err = runBPSOrgExecute(t, account)
	require.NoError(t, err)
	resp.Body.Close()
	got := BPSAccountStatuses(context.Background(), nil, []int64{account.ID()})[0]
	require.Equal(t, 1, got.BudgetUsed, "successes are counted with no budget set")
	require.Equal(t, 2, got.Attempts, "every attempt is counted")
	require.Zero(t, got.Budget)
	require.Zero(t, got.MaxConcurrency)
}
