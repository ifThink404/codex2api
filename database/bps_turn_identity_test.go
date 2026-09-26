package database

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func turnTestKey(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

func TestBPSTurnTaskFixedLifetimeAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	key := turnTestKey(t.Name())
	step := func(n int) string { return turnTestKey(fmt.Sprint(n)) }
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	first, reused, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, step(1), 24, clock)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, BPSTurnTaskIdentity{0, 24}, first)
	// Allocation without an upstream send does not start the lifetime.
	now = now.Add(48 * time.Hour)
	unsent, _, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, step(2), 2, clock)
	require.NoError(t, err)
	require.Equal(t, first, unsent)
	start := now
	activity, err := db.touchBPSTurnTaskIdentity(t.Context(), key, 0, now)
	require.NoError(t, err)
	require.Equal(t, start.UnixMilli(), activity.StartedAtMS)
	// Hourly traffic must not perpetually extend a task's deadline.
	for hour := 1; hour < 24; hour++ {
		now = start.Add(time.Duration(hour) * time.Hour)
		id, _, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, step(hour+2), 2, clock)
		require.NoError(t, err)
		require.Equal(t, first, id)
		activity, err = db.touchBPSTurnTaskIdentity(t.Context(), key, 0, now)
		require.NoError(t, err)
		require.Equal(t, start.UnixMilli(), activity.StartedAtMS)
		require.Equal(t, now.UnixMilli(), activity.LastSentAtMS)
	}
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	now = start.Add(24*time.Hour - time.Millisecond)
	before, _, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, step(100), 2, clock)
	require.NoError(t, err)
	require.Equal(t, first, before)
	now = start.Add(24 * time.Hour)
	next, _, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, step(101), 2, clock)
	require.NoError(t, err)
	require.Equal(t, BPSTurnTaskIdentity{1, 2}, next, "fixed deadline survives restarts and uses next-task settings")
	activity, err = db.touchBPSTurnTaskIdentity(t.Context(), key, 1, now)
	require.NoError(t, err)
	nextStart := activity.StartedAtMS
	now = now.Add(time.Hour)
	old, reused, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, step(1), 3, clock)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, first, old, "expired retries retain their assignment")
	_, err = db.touchBPSTurnTaskIdentity(t.Context(), key, 0, now)
	require.NoError(t, err)
	activity, err = db.touchBPSTurnTaskIdentity(t.Context(), key, 1, now)
	require.NoError(t, err)
	require.Equal(t, nextStart, activity.StartedAtMS)
	now = now.Add(time.Hour)
	third, _, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, step(102), 3, clock)
	require.NoError(t, err)
	require.Equal(t, BPSTurnTaskIdentity{2, 3}, third)
	other, _, err := db.resolveBPSTurnTaskIdentity(t.Context(), turnTestKey("other-model"), step(102), 24, clock)
	require.NoError(t, err)
	require.Equal(t, BPSTurnTaskIdentity{0, 24}, other)
	for _, hours := range []int{0, -1, MaxBPSTurnTaskLifetimeHours + 1} {
		_, _, err := db.ResolveBPSTurnTaskIdentity(t.Context(), key, step(103), hours)
		require.Error(t, err)
		require.Equal(t, DefaultBPSTurnTaskLifetimeHours, NormalizeBPSTurnTaskLifetimeHours(hours))
	}
}

func TestBPSTurnTaskConcurrentInstances(t *testing.T) {
	checkBPSTurnTaskConcurrentInstances(t, "sqlite", filepath.Join(t.TempDir(), "concurrent.db"))
}

func TestBPSTurnTaskPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	checkBPSTurnTaskConcurrentInstances(t, "postgres", dsn)
}

func checkBPSTurnTaskConcurrentInstances(t *testing.T, driver, dsn string) {
	t.Helper()
	first, err := New(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := New(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	key := turnTestKey(t.TempDir())
	now := time.Now()
	_, _, err = first.resolveBPSTurnTaskIdentity(t.Context(), key, turnTestKey("initial"), 1, func() time.Time { return now })
	require.NoError(t, err)
	_, err = first.touchBPSTurnTaskIdentity(t.Context(), key, 0, now)
	require.NoError(t, err)
	now = now.Add(time.Hour)
	type result struct {
		id     BPSTurnTaskIdentity
		reused bool
		err    error
	}
	results := make(chan result, 24)
	for i := range 24 {
		go func() {
			db := []*DB{first, second}[i%2]
			id, reused, err := db.resolveBPSTurnTaskIdentity(t.Context(), key, turnTestKey(fmt.Sprint(i/2)), 24, func() time.Time { return now })
			results <- result{id, reused, err}
		}()
	}
	newCount := 0
	for range 24 {
		r := <-results
		require.NoError(t, r.err)
		require.Equal(t, BPSTurnTaskIdentity{1, 24}, r.id, "concurrent expiry rotates once")
		if !r.reused {
			newCount++
		}
	}
	require.Equal(t, 12, newCount, "replays must not allocate twice across instances")
	activity, err := first.touchBPSTurnTaskIdentity(t.Context(), key, 1, now)
	require.NoError(t, err)
	later, err := second.touchBPSTurnTaskIdentity(t.Context(), key, 1, now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, activity.StartedAtMS, later.StartedAtMS)
	older, err := first.touchBPSTurnTaskIdentity(t.Context(), key, 1, now)
	require.NoError(t, err)
	require.Equal(t, later, older)
}

func TestBPSTurnTaskMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	settings := &SystemSettings{BPSTurnTaskLifetimeHours: 36, CodexFingerprintDefaultMode: "turn_round"}
	require.NoError(t, db.UpdateSystemSettings(t.Context(), settings))
	for _, table := range []string{"bps_turn_steps", "bps_turn_tasks", "bps_turn_batches"} {
		_, err := db.conn.Exec("DROP TABLE " + table)
		require.NoError(t, err)
	}
	_, err = db.conn.Exec("ALTER TABLE system_settings DROP COLUMN bps_turn_task_lifetime_hours")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	saved, err := db.GetSystemSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, 24, saved.BPSTurnTaskLifetimeHours)
	require.Equal(t, "turn_round", saved.CodexFingerprintDefaultMode)
	_, _, err = db.ResolveBPSTurnTaskIdentity(t.Context(), turnTestKey("account"), turnTestKey("step"), 24)
	require.NoError(t, err)
}
