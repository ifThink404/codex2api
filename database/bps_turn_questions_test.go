package database

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSTurnQuestionsLimitReplayAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "questions.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	key := turnTestKey("user/task")
	var first BPSRoundIdentity
	for n := 1; n <= 100; n++ {
		limit := 100
		if n > 1 {
			limit = 7 // A settings change must not cut an existing batch short.
		}
		q := turnTestKey(fmt.Sprint(n))
		id, reused, err := db.ResolveBPSTurnQuestionIdentity(t.Context(), key, q, limit)
		require.NoError(t, err)
		require.False(t, reused)
		require.Equal(t, BPSRoundIdentity{0, int64(n), 100, 0}, id)
		if n == 1 {
			first = id
		}
		// Tools, compaction and HTTP retries all reference the same question.
		for range 3 {
			again, reused, err := db.ResolveBPSTurnQuestionIdentity(t.Context(), key, q, limit)
			require.NoError(t, err)
			require.True(t, reused)
			require.Equal(t, id, again)
		}
	}
	next, reused, err := db.ResolveBPSTurnQuestionIdentity(t.Context(), key, turnTestKey("101"), 7)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, BPSRoundIdentity{1, 1, 7, 0}, next)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	old, reused, err := db.ResolveBPSTurnQuestionIdentity(t.Context(), key, turnTestKey("1"), 1)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, first, old, "late continuations retain the original question batch")
	// The task timer owns expiry; this counter must not inherit the other
	// mode's 24-hour idle rotation even if an activity row happens to exist.
	_, err = db.touchBPSRoundIdentity(t.Context(), key, 1, time.Now().Add(-48*time.Hour))
	require.NoError(t, err)
	next, _, err = db.ResolveBPSTurnQuestionIdentity(t.Context(), key, turnTestKey("102"), 1)
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{1, 2, 7, 0}, next)
	for _, limit := range []int{0, -1, MaxBPSTurnRoundLimit + 1} {
		_, _, err = db.ResolveBPSTurnQuestionIdentity(t.Context(), key, turnTestKey("invalid"), limit)
		require.Error(t, err)
		require.Equal(t, DefaultBPSTurnRoundLimit, NormalizeBPSTurnRoundLimit(limit))
	}
}

func TestBPSTurnQuestionsConcurrentInstances(t *testing.T) {
	checkBPSTurnQuestionConcurrency(t, "sqlite", filepath.Join(t.TempDir(), "concurrent.db"))
}

func TestBPSTurnQuestionsPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	checkBPSTurnQuestionConcurrency(t, "postgres", dsn)
}

func checkBPSTurnQuestionConcurrency(t *testing.T, driver, dsn string) {
	t.Helper()
	first, err := New(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := New(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	key := turnTestKey(t.TempDir())
	type result struct {
		question int
		id       BPSRoundIdentity
		reused   bool
		err      error
	}
	results := make(chan result, 24)
	for i := range 24 {
		go func() {
			id, reused, err := []*DB{first, second}[i%2].ResolveBPSTurnQuestionIdentity(t.Context(), key, turnTestKey(fmt.Sprint(i/2)), 3)
			results <- result{i / 2, id, reused, err}
		}()
	}
	questions := map[int]BPSRoundIdentity{}
	positions := map[BPSRoundIdentity]bool{}
	newCount := 0
	for range 24 {
		r := <-results
		require.NoError(t, r.err)
		require.Equal(t, 3, r.id.RoundLimit)
		require.GreaterOrEqual(t, r.id.Iteration, int64(1))
		require.LessOrEqual(t, r.id.Iteration, int64(3))
		if previous, ok := questions[r.question]; ok {
			require.Equal(t, previous, r.id)
		} else {
			require.False(t, positions[r.id], "different questions cannot share a counter position")
			questions[r.question], positions[r.id] = r.id, true
		}
		if !r.reused {
			newCount++
		}
	}
	require.Len(t, positions, 12)
	require.Equal(t, 12, newCount)
}
