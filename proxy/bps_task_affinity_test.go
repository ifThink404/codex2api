package proxy

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBPSTaskAffinityReusesSwitchesAndSurvivesRestart(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "task.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	makeHandler := func() *Handler {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, AffinityMode: "strict"})
		t.Cleanup(store.Stop)
		for _, id := range []int64{1, 2} {
			store.AddAccount(&auth.Account{DBID: id, AccessToken: "test", AccountID: "upstream", Models: []string{"gpt-6-astra"}, CodexBPS: true, CodexFingerprintMode: "session"})
		}
		return &Handler{db: db, store: store}
	}
	h := makeHandler()
	request := func(body, user string) *gin.Context {
		c := inferredSessionFixture(t, body, user, "device", "client/1", 8)
		beginDispatchSelection(c)
		return c
	}
	pick := func(c *gin.Context, allow int64) *bpsTaskAffinityDiagnostic {
		affinity := capacityAwareSessionAffinityKey(requestSessionIdentity{affinityID: "untrusted"}, 8)
		filter := func(a *auth.Account) bool { return allow == 0 || a.ID() == allow }
		a, _, _ := h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), affinity, 8, &retryAccountExclusions{}, filter, auth.DispatchPolicyStandard.WithModel("gpt-6-astra"))
		require.NotNil(t, a)
		h.store.Release(a)
		input := &database.UsageLogInput{AccountID: a.ID()}
		populateUsageRequestDiagnostics(c, input)
		d := readUsageDiagnosticSnapshot(t, input).BPSTaskAffinity
		require.NotNil(t, d)
		return d
	}
	first := pick(request(inferredOpening, "alice"), 1)
	require.Equal(t, "bound", first.Result)
	require.True(t, first.Persisted)
	h = makeHandler() // A fresh scheduler has no process-local affinity.
	next := pick(request(inferredFollowup, "alice"), 0)
	require.Equal(t, int64(1), next.SelectedAccountID)
	require.Equal(t, "reused", next.Result)
	require.Equal(t, first.TaskKey, next.TaskKey)
	other := pick(request(inferredOpening, "bob"), 2)
	require.NotEqual(t, first.TaskKey, other.TaskKey)
	require.Equal(t, int64(2), other.SelectedAccountID)
	switched := pick(request(inferredFollowup, "alice"), 2)
	require.Equal(t, "switched", switched.Result)
	require.Equal(t, int64(1), switched.PreferredAccountID)
	require.Equal(t, int64(2), switched.SelectedAccountID)
	next = pick(request(inferredOpening, "alice"), 0)
	require.Equal(t, "reused", next.Result)
	require.Equal(t, int64(2), next.SelectedAccountID)
	changed := pick(request(strings.Replace(inferredOpening, "clock", "calendar", 1), "alice"), 1)
	require.NotEqual(t, first.TaskKey, changed.TaskKey)
	// The preference remains soft when the selected account has no free slots.
	heldOne := h.store.TakePreferredAccountWithDispatch(2, 8, nil, nil, auth.DispatchPolicyStandard.WithModel("gpt-6-astra"))
	heldTwo := h.store.TakePreferredAccountWithDispatch(2, 8, nil, nil, auth.DispatchPolicyStandard.WithModel("gpt-6-astra"))
	require.NotNil(t, heldOne)
	require.NotNil(t, heldTwo)
	busy := pick(request(inferredOpening, "alice"), 0)
	require.Equal(t, "switched", busy.Result)
	require.Equal(t, int64(1), busy.SelectedAccountID)
	h.store.Release(heldOne)
	h.store.Release(heldTwo)
}

func TestBPSTaskAffinityExplicitTaskAndIdentityBoundaries(t *testing.T) {
	body := `{"model":"gpt-6-astra","metadata":{"task_id":"` + testRootSessionA + `"},"input":"hello"}`
	first := inferredSessionFixture(t, body, "alice", "device", "client/1", 8)
	state := first.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	require.Equal(t, "client_task_id", state.diagnostic.Source)
	next := inferredSessionFixture(t, strings.Replace(body, "hello", "next turn", 1), "alice", "device", "client/2", 8)
	require.Equal(t, state.seed, next.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession).seed)
	other := inferredSessionFixture(t, body, "bob", "device", "client/1", 8)
	require.NotEqual(t, state.seed, other.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession).seed)
	// A native identity always keeps precedence over task hints.
	bindInferredBPSSession(first, []byte(body), requestSessionIdentity{explicitUpstreamID: testRootSessionA}, requestRootSessionIdentity{}, verifiedNewAPIPolicyContext{}, false)
	require.Empty(t, first.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession).seed)
}

func TestBPSTaskAffinityHonorsDisabledModesAndExclusions(t *testing.T) {
	for _, mode := range []string{"session", "full", "round", "turn_round", "device", "off"} {
		t.Run(mode, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "task.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, AffinityMode: "strict"})
			t.Cleanup(store.Stop)
			for _, id := range []int64{1, 2} {
				store.AddAccount(&auth.Account{DBID: id, AccessToken: "test", Models: []string{"gpt-6-astra"}, CodexBPS: true, CodexFingerprintMode: mode})
			}
			h := &Handler{db: db, store: store}
			c := inferredSessionFixture(t, inferredOpening, "alice", "device", "client/1", 8)
			beginDispatchSelection(c)
			state := c.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
			key := codexIdentityDigest("bps-task-affinity-v1", state.seed)
			_, err = db.UpdateBPSTaskAffinity(t.Context(), key, 0, 1)
			require.NoError(t, err)
			a, _, _ := h.nextAccountForBPSTask(c.Request.Context(), "", 8, map[int64]bool{1: true}, nil, auth.DispatchPolicyStandard.WithModel("gpt-6-astra"))
			require.NotNil(t, a)
			require.Equal(t, int64(2), a.ID())
			store.Release(a)
			stored, err := db.ReadBPSTaskAffinity(t.Context(), key)
			require.NoError(t, err)
			if mode == "session" || mode == "full" || mode == "round" || mode == "turn_round" {
				require.Equal(t, int64(2), stored.AccountID)
			} else {
				require.Equal(t, int64(1), stored.AccountID, "disabled convergence must not update the preference")
			}
			store.SetAffinityMode("off")
			c = inferredSessionFixture(t, inferredOpening, "alice", "device", "client/1", 8)
			beginDispatchSelection(c)
			a, _, _ = h.nextAccountForBPSTask(c.Request.Context(), "", 8, nil, nil, auth.DispatchPolicyStandard.WithModel("gpt-6-astra"))
			require.NotNil(t, a)
			store.Release(a)
			require.Nil(t, c.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession).affinity)
		})
	}
}
