package proxy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

func TestBPSPolicyBlockHistoryFollowsStrikesAndProbes(t *testing.T) {
	freshBPSAccountStates(t)
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "history.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	account := bpsOrgAccount(9901)
	h := bpsProbeHandler(t, account)
	h.db = db
	strike := func() {
		t.Helper()
		req := plugins.NewRequest("req-strike", plugins.KindResponses, nil, nil, 0)
		req.SetState(BPSPluginID, &bpsRequest{handler: h})
		env := &plugins.ReqEnv{Request: req, Account: account, Model: "gpt-6-sol"}
		bpsPlugin{}.FilterHeaders(env, http.Header{})
		_, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsPolicyBlockBody))
		require.NoError(t, err)
	}
	strike()
	strike()
	active, _, err := db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Empty(t, active, "strikes below the trigger are not a block yet")
	firstStrike := time.Now()
	strike()
	active, _, err = db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, active, 1, "the trigger opens a block event")
	require.Equal(t, 1, active[0].Tier)
	require.WithinDuration(t, firstStrike, active[0].BlockedAt, 3*time.Second, "blocked_at is the first strike")

	// A blocked probe escalates the event; a successful one closes it.
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		return http.StatusForbidden, "application/json", bpsPolicyBlockBody
	}}
	installBPSOrgUpstream(t, account, u)
	expire := func() {
		bpsAccountStateStore.update(context.Background(), nil, bpsAccountStateKey(account.ID()), time.Now(), func(r *bpsAccountRecord) {
			r.Until = time.Now().Add(-time.Second)
		})
	}
	expire()
	h.runBPSPolicyProbes(context.Background(), time.Now())
	active, _, err = db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 2, active[0].Tier)
	require.Equal(t, 1, active[0].ProbeCount)
	require.Equal(t, bpsProbeBlocked, active[0].LastProbeResult)

	u.respond = func(int32) (int, string, string) {
		return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamComplete)
	}
	expire()
	h.runBPSPolicyProbes(context.Background(), time.Now())
	active, history, err := db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Empty(t, active)
	require.Len(t, history, 1)
	require.NotNil(t, history[0].ClearedAt)
	require.Equal(t, 2, history[0].ProbeCount)
	require.Equal(t, bpsProbeOK, history[0].LastProbeResult)
	require.InDelta(t, time.Since(history[0].BlockedAt).Seconds(), float64(history[0].DurationSeconds), 3, "duration = cleared_at - blocked_at")
}
