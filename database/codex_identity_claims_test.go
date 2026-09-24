package database

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexIdentityClaimsPersistAndRollback(test *testing.T) {
	path := filepath.Join(test.TempDir(), "claims.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	owner, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	first, fresh := strings.Repeat("2", 64), strings.Repeat("1", 64)
	ctx := context.Background()
	require.NoError(test, db.ClaimCodexIdentities(ctx, []string{first, first}, owner))
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	require.NoError(test, db.ClaimCodexIdentities(ctx, []string{first}, owner))
	conflictErr := db.ClaimCodexIdentities(ctx, []string{fresh, first}, other)
	require.ErrorIs(test, conflictErr, ErrCodexIdentityConflict)
	var conflict *CodexIdentityConflictError
	require.ErrorAs(test, conflictErr, &conflict)
	require.Equal(test, first, conflict.IdentityKey)
	require.Equal(test, owner, conflict.ExistingOwner)
	require.NotContains(test, conflictErr.Error(), owner)
	require.NoError(test, db.ClaimCodexIdentities(ctx, []string{fresh}, owner))
	require.Error(test, db.ClaimCodexIdentities(ctx, []string{"raw-private-id"}, owner))
	var count int
	require.NoError(test, db.conn.QueryRow(`SELECT count(*) FROM codex_identity_claims`).Scan(&count))
	require.Equal(test, 2, count)
}

func TestCodexIdentityClaimsConcurrentOwners(test *testing.T) {
	path := filepath.Join(test.TempDir(), "claims.db")
	first, err := New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, first.Close()) })
	second, err := New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, second.Close()) })
	results := make(chan error, 8)
	start := make(chan struct{})
	for index := 0; index < 8; index++ {
		db := first
		if index%2 == 1 {
			db = second
		}
		go func() {
			<-start
			results <- db.ClaimCodexIdentities(context.Background(), []string{strings.Repeat("c", 64), strings.Repeat("d", 64)}, fmt.Sprintf("%064x", index+1))
		}()
	}
	close(start)
	success := 0
	for index := 0; index < 8; index++ {
		if err := <-results; err == nil {
			success++
		} else {
			require.ErrorIs(test, err, ErrCodexIdentityConflict)
		}
	}
	require.Equal(test, 1, success)
}
