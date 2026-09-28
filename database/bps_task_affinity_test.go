package database

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSTaskAffinityPersistsAndRejectsStaleSwitches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "affinity.db")
	db, err := newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	key := strings.Repeat("a", 64)
	record, err := db.ReadBPSTaskAffinity(t.Context(), key)
	require.NoError(t, err)
	require.Zero(t, record.AccountID)
	first, err := db.UpdateBPSTaskAffinity(t.Context(), key, 0, 1)
	require.NoError(t, err)
	same, err := db.UpdateBPSTaskAffinity(t.Context(), key, first.Revision, 1)
	require.NoError(t, err)
	require.Equal(t, first, same, "reaffirming the same account must not create a new turn epoch")
	second, err := db.UpdateBPSTaskAffinity(t.Context(), key, 0, 2)
	require.NoError(t, err)
	require.Equal(t, first, second, "a concurrent first request must not overwrite the winner")
	next, err := db.UpdateBPSTaskAffinity(t.Context(), key, first.Revision, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), next.AccountID)
	require.Equal(t, first.Revision+1, next.Revision)
	stale, err := db.UpdateBPSTaskAffinity(t.Context(), key, first.Revision, 3)
	require.NoError(t, err)
	require.Equal(t, next, stale)
	require.NoError(t, db.Close())
	db, err = newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	stored, err := db.ReadBPSTaskAffinity(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, next, stored)
	other, err := db.ReadBPSTaskAffinity(t.Context(), strings.Repeat("b", 64))
	require.NoError(t, err)
	require.Zero(t, other.AccountID)
}
