package database

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexIdentityReferencesPersistAndFreezeAcrossGenerations(test *testing.T) {
	path := filepath.Join(test.TempDir(), "references.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	identity, reference := strings.Repeat("a", 64), strings.Repeat("b", 64)
	first := CodexIdentityEpoch{RootKey: strings.Repeat("c", 24), Generation: 1, Segment: strings.Repeat("d", 64)}
	require.NoError(test, db.PublishCodexIdentityEpoch(test.Context(), identity, first))
	resolved, found, bound, err := db.ReadCodexIdentityReference(test.Context(), reference, identity)
	require.NoError(test, err)
	require.True(test, found)
	require.Equal(test, first, resolved)
	require.False(test, bound)
	require.NoError(test, db.ClaimCodexIdentityReference(test.Context(), reference, resolved))
	next := first
	next.Generation, next.Segment = 3, strings.Repeat("e", 64)
	require.NoError(test, db.PublishCodexIdentityEpoch(test.Context(), identity, next))
	require.ErrorIs(test, db.PublishCodexIdentityEpoch(test.Context(), identity, first), ErrSessionOwnerConflict)
	require.ErrorIs(test, db.ClaimCodexIdentityReference(test.Context(), reference, next), ErrCodexIdentityConflict)
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	resolved, found, bound, err = db.ReadCodexIdentityReference(test.Context(), reference, identity)
	require.NoError(test, err)
	require.True(test, found)
	require.Equal(test, first, resolved)
	require.True(test, bound)
	resolved, found, bound, err = db.ReadCodexIdentityReference(test.Context(), strings.Repeat("f", 64), identity)
	require.NoError(test, err)
	require.True(test, found)
	require.Equal(test, next, resolved)
	require.False(test, bound)
}

func TestCodexIdentityReferenceConcurrentClaimHasSingleWinner(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "reference-race.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	first := CodexIdentityEpoch{Generation: 1, Segment: strings.Repeat("a", 64)}
	second := CodexIdentityEpoch{Generation: 2, Segment: strings.Repeat("b", 64)}
	reference := strings.Repeat("c", 64)
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, epoch := range []CodexIdentityEpoch{first, second} {
		group.Go(func() { results <- db.ClaimCodexIdentityReference(test.Context(), reference, epoch) })
	}
	group.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			require.ErrorIs(test, err, ErrCodexIdentityConflict)
		}
	}
	require.Equal(test, 1, winners)
	_, found, _, err := db.ReadCodexIdentityReference(test.Context(), reference, strings.Repeat("d", 64))
	require.NoError(test, err)
	require.True(test, found)
}

func TestCodexDetachedReferencePersistsAndCannotReplaceParentEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "detached.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	identity, reference := strings.Repeat("a", 64), strings.Repeat("b", 64)
	detached := CodexIdentityEpoch{Detached: true}
	require.NoError(t, db.ClaimCodexIdentityReference(t.Context(), reference, detached))
	require.Error(t, db.PublishCodexIdentityEpoch(t.Context(), identity, detached))
	require.Error(t, db.ClaimCodexIdentityReference(t.Context(), strings.Repeat("c", 64), CodexIdentityEpoch{Detached: true, Generation: 1}))
	parent := CodexIdentityEpoch{RootKey: strings.Repeat("d", 24)}
	require.NoError(t, db.PublishCodexIdentityEpoch(t.Context(), identity, parent))
	require.ErrorIs(t, db.ClaimCodexIdentityReference(t.Context(), reference, parent), ErrCodexIdentityConflict)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	resolved, found, bound, err := db.ReadCodexIdentityReference(t.Context(), reference, identity)
	require.NoError(t, err)
	require.True(t, found && bound)
	require.Equal(t, detached, resolved)
	resolved, found, bound, err = db.ReadCodexIdentityReference(t.Context(), strings.Repeat("e", 64), identity)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, bound)
	require.Equal(t, parent, resolved, "detaching a child cannot modify the actual parent")
}
