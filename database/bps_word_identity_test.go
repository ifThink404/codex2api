package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSWordIterationInstancesAndRestart(t *testing.T) {
	checkBPSWordIteration(t, "sqlite", filepath.Join(t.TempDir(), "word.db"))
}

func TestBPSWordIterationPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	checkBPSWordIteration(t, "postgres", dsn)
}

func checkBPSWordIteration(t *testing.T, driver, dsn string) {
	t.Helper()
	first, err := New(driver, dsn)
	require.NoError(t, err)
	second, err := New(driver, dsn)
	require.NoError(t, err)
	// The timestamped directory name also isolates repeated PostgreSQL runs.
	digest := sha256.Sum256([]byte("word-turn:" + t.TempDir()))
	key := hex.EncodeToString(digest[:])
	step := strings.Repeat("b", 64)
	type result struct {
		n      int64
		reused bool
		err    error
	}
	results := make(chan result, 2)
	for _, db := range []*DB{first, second} {
		go func() {
			n, reused, err := db.ResolveBPSWordIteration(context.Background(), key, step)
			results <- result{n, reused, err}
		}()
	}
	left, right := <-results, <-results
	require.NoError(t, left.err)
	require.NoError(t, right.err)
	require.EqualValues(t, 1, left.n)
	require.EqualValues(t, 1, right.n)
	require.NotEqual(t, left.reused, right.reused)
	n, reused, err := first.ResolveBPSWordIteration(t.Context(), key, strings.Repeat("c", 64))
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	require.False(t, reused)
	require.NoError(t, first.Close())
	require.NoError(t, second.Close())
	resumed, err := New(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resumed.Close()) })
	n, reused, err = resumed.ResolveBPSWordIteration(t.Context(), key, strings.Repeat("c", 64))
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	require.True(t, reused)
	digest = sha256.Sum256([]byte(key + ":next"))
	n, _, err = resumed.ResolveBPSWordIteration(t.Context(), hex.EncodeToString(digest[:]), step)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}
