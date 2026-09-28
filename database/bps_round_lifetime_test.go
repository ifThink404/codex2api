package database

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSRoundLifetimeMigrationAndSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifetime-migration.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	key, step := turnTestKey(t.Name()), turnTestKey("original")
	settings := &SystemSettings{BPSRoundTaskLifetimeHours: 36, BPSTurnTaskLifetimeHours: 48, BPSRoundConvergenceLimit: 200}
	require.NoError(t, db.UpdateSystemSettings(t.Context(), settings))
	saved, err := db.GetSystemSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, 36, saved.BPSRoundTaskLifetimeHours)
	require.Equal(t, 48, saved.BPSTurnTaskLifetimeHours)
	old, _, err := db.ResolveBPSRoundIdentity(t.Context(), key, step, 200, 24)
	require.NoError(t, err)
	now := time.Now()
	_, err = db.touchBPSRoundIdentity(t.Context(), key, 0, now.Add(-23*time.Hour))
	require.NoError(t, err)
	for _, table := range []string{"bps_round_tasks", "bps_round_steps"} {
		_, err = db.conn.Exec("ALTER TABLE " + table + " DROP COLUMN lifetime_hours")
		require.NoError(t, err)
	}
	_, err = db.conn.Exec("ALTER TABLE system_settings DROP COLUMN bps_round_task_lifetime_hours")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	saved, err = db.GetSystemSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, 24, saved.BPSRoundTaskLifetimeHours)
	require.Equal(t, 48, saved.BPSTurnTaskLifetimeHours)
	retry, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step, 10, 6)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, old, retry, "schema migration retains old assignments")
	next, _, err := db.resolveBPSRoundIdentity(t.Context(), key, turnTestKey("before expiry"), 10, 6, func() time.Time { return now })
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{0, 2, 200, 24}, next)
	now = now.Add(time.Hour)
	next, _, err = db.resolveBPSRoundIdentity(t.Context(), key, turnTestKey("at expiry"), 10, 6, func() time.Time { return now })
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{1, 1, 10, 6}, next)
}

func TestBPSRoundLifetimeConcurrentExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expiry-concurrent.db")
	first, err := New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	key := turnTestKey(t.Name())
	_, _, err = first.ResolveBPSRoundIdentity(t.Context(), key, turnTestKey("initial"), 100, 1)
	require.NoError(t, err)
	_, err = first.touchBPSRoundIdentity(t.Context(), key, 0, time.Now().Add(-2*time.Hour))
	require.NoError(t, err)
	type result struct {
		value  BPSRoundIdentity
		reused bool
		err    error
	}
	results := make(chan result, 24)
	for i := range 24 {
		go func() {
			db := []*DB{first, second}[i%2]
			id, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, turnTestKey(fmt.Sprint(i/2)), 100, 6)
			results <- result{id, reused, err}
		}()
	}
	positions := map[int64]int{}
	allocations := 0
	for range 24 {
		r := <-results
		require.NoError(t, r.err)
		require.EqualValues(t, 1, r.value.Generation, "concurrent expiry rotates only once")
		require.Equal(t, 6, r.value.LifetimeHours)
		positions[r.value.Iteration]++
		if !r.reused {
			allocations++
		}
	}
	require.Equal(t, 12, allocations)
	for i := int64(1); i <= 12; i++ {
		require.Equal(t, 2, positions[i])
	}
}
