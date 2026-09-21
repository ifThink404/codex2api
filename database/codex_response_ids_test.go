package database

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResponseIDMappingPersistenceIsolationAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response-ids.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	binding := CodexTurnStateBinding{Scope: "user", RootKey: "root", AccountID: 71, AccountHash: "official-account"}
	r, err := db.IssueCodexResponseID(t.Context(), binding, "resp_original")
	require.NoError(t, err)
	require.True(t, db.IsManagedCodexResponseID(r.Alias))
	require.Len(t, r.Alias, 69)
	require.NotEqual(t, r.Real, r.Alias)
	again, err := db.IssueCodexResponseID(t.Context(), binding, r.Real)
	require.NoError(t, err)
	require.Equal(t, r.Alias, again.Alias)
	var ciphertext string
	require.NoError(t, db.conn.QueryRow(`SELECT ciphertext FROM codex_response_ids WHERE alias=$1`, r.Alias).Scan(&ciphertext))
	require.NotContains(t, ciphertext, r.Real)
	for _, changed := range []CodexTurnStateBinding{
		{Scope: "other-user", RootKey: "root", AccountID: 71, AccountHash: "official-account"},
		{Scope: "user", RootKey: "other-root", AccountID: 71, AccountHash: "official-account"},
		{Scope: "user", RootKey: "root", AccountID: 72, AccountHash: "other-account"},
		{Scope: "user", RootKey: "root", AccountID: 71, AccountHash: "official-account", Generation: 2},
	} {
		other, err := db.IssueCodexResponseID(t.Context(), changed, r.Real)
		require.NoError(t, err)
		require.NotEqual(t, r.Alias, other.Alias)
	}
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	read, found, err := db.ReadCodexResponseID(t.Context(), r.Alias)
	require.NoError(t, err)
	require.True(t, found)
	// Reissuing renews the TTL; a second boundary may separate r and again.
	require.Equal(t, again, read)
	_, found, err = db.ReadCodexResponseID(t.Context(), "resp_"+strings.Repeat("f", 64))
	require.NoError(t, err)
	require.False(t, found)
	require.False(t, db.IsManagedCodexTurnStateAlias(r.Alias))
	_, err = db.conn.Exec(`UPDATE codex_response_ids SET expires_at=$1 WHERE alias=$2`, time.Now().Add(-time.Second).Unix(), r.Alias)
	require.NoError(t, err)
	_, found, err = db.ReadCodexResponseID(t.Context(), r.Alias)
	require.NoError(t, err)
	require.False(t, found)
}

func TestResponseIDMappingSharedInstancesAndLostKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response-ids.db")
	first, err := New("sqlite", path)
	require.NoError(t, err)
	second, err := New("sqlite", path)
	require.NoError(t, err)
	binding := CodexTurnStateBinding{Scope: "user", RootKey: "root", AccountID: 7}
	results := make(chan CodexResponseIDRecord, 8)
	failures := make(chan error, 8)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			db := first
			if i%2 == 1 {
				db = second
			}
			r, err := db.IssueCodexResponseID(context.Background(), binding, "resp_shared")
			results <- r
			failures <- err
		}(i)
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	alias := ""
	for r := range results {
		if alias == "" {
			alias = r.Alias
		}
		require.Equal(t, alias, r.Alias)
	}
	read, found, err := second.ReadCodexResponseID(t.Context(), alias)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "resp_shared", read.Real)
	_, err = first.conn.Exec(`DELETE FROM codex_turn_state_secret`)
	require.NoError(t, err)
	require.NoError(t, second.Close())
	require.NoError(t, first.Close())
	_, err = New("sqlite", path)
	require.ErrorContains(t, err, "encryption key missing")
}
