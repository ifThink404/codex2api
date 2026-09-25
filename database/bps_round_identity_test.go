package database

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSRoundIdentityRotationAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rounds.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name())))
	step := func(n int) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(n)))) }
	var first BPSRoundIdentity
	for i := 1; i <= 6; i++ {
		limit := 3
		if i > 1 {
			limit = 2
		}
		value, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step(i), limit)
		require.NoError(t, err)
		require.False(t, reused)
		if i == 1 {
			first = value
		}
		want := []BPSRoundIdentity{{0, 1, 3}, {0, 2, 3}, {0, 3, 3}, {1, 1, 2}, {1, 2, 2}, {2, 1, 2}}[i-1]
		require.Equal(t, want, value)
	}
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	old, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step(1), 1)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, first, old, "an old retry must not migrate into the latest batch")
	next, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step(7), 1)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, BPSRoundIdentity{2, 2, 2}, next, "restart retains both counter and active batch limit")
	otherKey := fmt.Sprintf("%x", sha256.Sum256([]byte("another account")))
	other, _, err := db.ResolveBPSRoundIdentity(t.Context(), otherKey, step(1), 3)
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{0, 1, 3}, other)
	_, _, err = db.ResolveBPSRoundIdentity(t.Context(), key, step(9), 0)
	require.Error(t, err)
}

func TestBPSRoundIdentityConcurrentInstances(t *testing.T) {
	checkBPSRoundConcurrentInstances(t, "sqlite", filepath.Join(t.TempDir(), "concurrent.db"))
}

func TestBPSRoundIdentityPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	checkBPSRoundConcurrentInstances(t, "postgres", dsn)
}

func checkBPSRoundConcurrentInstances(t *testing.T, driver, dsn string) {
	t.Helper()
	first, err := New(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := New(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(t.TempDir())))
	type result struct {
		step   int
		value  BPSRoundIdentity
		reused bool
		err    error
	}
	results := make(chan result, 24)
	for i := range 24 {
		go func() {
			db := []*DB{first, second}[i%2]
			step := i / 2
			stepKey := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(step))))
			value, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, stepKey, 3)
			results <- result{step, value, reused, err}
		}()
	}
	byStep := map[int]BPSRoundIdentity{}
	positions := map[BPSRoundIdentity]bool{}
	newCount := 0
	for range 24 {
		r := <-results
		require.NoError(t, r.err)
		if prior, ok := byStep[r.step]; ok {
			require.Equal(t, prior, r.value)
		} else {
			require.False(t, positions[r.value], "different steps must receive different positions")
			positions[r.value], byStep[r.step] = true, r.value
		}
		if !r.reused {
			newCount++
		}
	}
	require.Equal(t, 12, newCount)
	for generation := range int64(4) {
		for iteration := int64(1); iteration <= 3; iteration++ {
			require.True(t, positions[BPSRoundIdentity{generation, iteration, 3}])
		}
	}
}
