package proxy

import (
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func newBPSAffinityTestHandler(t *testing.T, id int64) (*Handler, *database.DB, *auth.Account) {
	t.Helper()
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "affinity.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.5", MaxRetries: 1})
	t.Cleanup(store.Stop)
	account := withBPSOverride((&auth.Account{DBID: id, AccessToken: "at", AccountID: "affinity-account"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceSession}), true)
	store.AddAccount(account)
	return NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil), db, account
}

func bindBPSAffinity(t *testing.T, h *Handler, seed string, heuristic bool, account *auth.Account) *inferredBPSSession {
	t.Helper()
	state := &inferredBPSSession{seed: seed, diagnostic: inferredBPSSessionDiagnostic{Result: "derived", Heuristic: heuristic}}
	h.bpsPreferredTaskAccount(t.Context(), state, "gpt-5.5")
	h.rememberBPSTaskAccount(t.Context(), state, account, "gpt-5.5")
	return state
}

func TestBPSHeuristicAffinityPersistenceSwitch(t *testing.T) {
	for _, persist := range []bool{true, false} {
		t.Run(map[bool]string{true: "persisted", false: "local"}[persist], func(t *testing.T) {
			value := persist
			updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.PersistHeuristicAffinity = &value; return c })
			h, db, account := newBPSAffinityTestHandler(t, 85001+map[bool]int64{true: 0, false: 1}[persist])
			heuristicSeed, clientSeed := NewUpstreamSessionUUID(), NewUpstreamSessionUUID()

			state := bindBPSAffinity(t, h, heuristicSeed, true, account)
			require.True(t, state.affinity.Persisted)
			stored, err := db.ReadBPSTaskAffinity(t.Context(), codexIdentityDigest("bps-task-affinity-v1", heuristicSeed))
			require.NoError(t, err)
			if persist {
				require.Equal(t, "database", state.affinity.Store)
				require.Equal(t, account.ID(), stored.AccountID, "fj behavior: heuristic seeds are shared by every replica")
			} else {
				require.Equal(t, "local", state.affinity.Store)
				require.Zero(t, stored.AccountID, "heuristic seeds stay out of the database")
			}
			// The preference is found again either way on this replica.
			next := &inferredBPSSession{seed: heuristicSeed, diagnostic: inferredBPSSessionDiagnostic{Heuristic: true}}
			require.Equal(t, account.ID(), h.bpsPreferredTaskAccount(t.Context(), next, "gpt-5.5"))

			// Client task IDs identify one conversation and always persist.
			client := bindBPSAffinity(t, h, clientSeed, false, account)
			require.Equal(t, "database", client.affinity.Store)
			stored, err = db.ReadBPSTaskAffinity(t.Context(), codexIdentityDigest("bps-task-affinity-v1", clientSeed))
			require.NoError(t, err)
			require.Equal(t, account.ID(), stored.AccountID)
		})
	}
}

func TestBPSLocalTaskAffinityMirrorsDatabaseRevisions(t *testing.T) {
	store := &bpsLocalTaskAffinityStore{}
	record, err := store.UpdateBPSTaskAffinity(t.Context(), "k", 0, 7)
	require.NoError(t, err)
	require.Equal(t, int64(1), record.Revision)
	record, _ = store.UpdateBPSTaskAffinity(t.Context(), "k", 1, 7)
	require.Equal(t, int64(1), record.Revision, "the same account keeps its revision")
	record, _ = store.UpdateBPSTaskAffinity(t.Context(), "k", 1, 8)
	require.Equal(t, database.BPSTaskAffinity{AccountID: 8, Revision: 2}, record)
	record, _ = store.UpdateBPSTaskAffinity(t.Context(), "k", 1, 9)
	require.Equal(t, int64(8), record.AccountID, "a stale revision cannot overwrite a newer binding")
	_, err = store.UpdateBPSTaskAffinity(t.Context(), "k", 0, 0)
	require.Error(t, err)
	for i := 0; i <= bpsLocalTaskAffinityLimit; i++ {
		_, _ = store.UpdateBPSTaskAffinity(t.Context(), NewUpstreamSessionUUID(), 0, 1)
	}
	require.Len(t, store.entries, bpsLocalTaskAffinityLimit)
	record, _ = store.ReadBPSTaskAffinity(t.Context(), "k")
	require.Zero(t, record.AccountID, "the oldest entry was evicted")
}
