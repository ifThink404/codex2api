package database

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestProjectIdentityPersistenceAndScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	a := "01a07f21-24a6-7ee2-b095-f0f5fdaee3d3"
	binding := CodexTurnStateBinding{Scope: "caller-A", RootKey: "thread-1", AccountID: 13, AccountHash: "account-13", Generation: 2}
	_, err = db.ResolveCodexProjectID(t.Context(), binding, a)
	require.Error(t, err)
	require.NoError(t, db.RegisterCodexProjectID(t.Context(), binding.Scope, a))
	var wg sync.WaitGroup
	results := make(chan CodexProtocolPair, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := db.ResolveCodexProjectID(t.Context(), binding, a)
			results <- p
			failures <- e
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for e := range failures {
		require.NoError(t, e)
	}
	var pair CodexProtocolPair
	for p := range results {
		if pair.Public == "" {
			pair = p
		}
		require.Equal(t, pair, p)
	}
	require.NotEqual(t, a, pair.Upstream)
	id, err := uuid.Parse(pair.Upstream)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), id.Version())
	var encrypted string
	require.NoError(t, db.conn.QueryRow(`SELECT ciphertext FROM codex_protocol_ids`).Scan(&encrypted))
	require.NotContains(t, encrypted, a)
	require.NotContains(t, encrypted, pair.Upstream)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	binding.RootKey = "thread-2"
	binding.Generation = 99
	restored, found, err := db.RestoreCodexProjectID(t.Context(), binding, pair.Upstream)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, pair, restored)
	stable, err := db.ResolveCodexProjectID(t.Context(), binding, a)
	require.NoError(t, err)
	require.Equal(t, pair, stable)
	for _, change := range []string{"owner", "account", "hash"} {
		t.Run(change, func(t *testing.T) {
			other := binding
			switch change {
			case "owner":
				other.Scope = "caller-B"
			case "account":
				other.AccountID++
			case "hash":
				other.AccountHash = "replaced-account"
			}
			_, found, err := db.RestoreCodexProjectID(t.Context(), other, pair.Upstream)
			require.NoError(t, err)
			require.False(t, found)
			require.NoError(t, db.RegisterCodexProjectID(t.Context(), other.Scope, a))
			next, err := db.ResolveCodexProjectID(t.Context(), other, a)
			require.NoError(t, err)
			require.NotEqual(t, pair.Upstream, next.Upstream)
		})
	}
}
