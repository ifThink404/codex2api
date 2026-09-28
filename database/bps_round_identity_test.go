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

func TestBPSRoundIdentityRotationAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rounds.db")
	db, err := newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name())))
	step := func(n int) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(n)))) }
	var first BPSRoundIdentity
	for i := 1; i <= 6; i++ {
		limit := 3
		if i > 1 {
			limit = 2
		}
		value, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step(i), limit, 24)
		require.NoError(t, err)
		require.False(t, reused)
		if i == 1 {
			first = value
		}
		want := []BPSRoundIdentity{{0, 1, 3, 24}, {0, 2, 3, 24}, {0, 3, 3, 24}, {1, 1, 2, 24}, {1, 2, 2, 24}, {2, 1, 2, 24}}[i-1]
		require.Equal(t, want, value)
	}
	require.NoError(t, db.Close())
	db, err = newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	old, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step(1), 1, 24)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, first, old, "an old retry must not migrate into the latest batch")
	next, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step(7), 1, 24)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, BPSRoundIdentity{2, 2, 2, 24}, next, "restart retains both counter and active batch limit")
	otherKey := fmt.Sprintf("%x", sha256.Sum256([]byte("another account")))
	other, _, err := db.ResolveBPSRoundIdentity(t.Context(), otherKey, step(1), 3, 24)
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{0, 1, 3, 24}, other)
	_, _, err = db.ResolveBPSRoundIdentity(t.Context(), key, step(9), 0, 24)
	require.Error(t, err)
}

func TestBPSRoundIdentityConcurrentInstances(t *testing.T) {
	checkBPSRoundConcurrentInstances(t, "sqlite", filepath.Join(t.TempDir(), "concurrent.db"))
}

func TestBPSRoundIdentityCustomLifetimeAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expiry.db")
	db, err := newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	key, otherKey := turnTestKey(t.Name()), turnTestKey(t.Name()+"/other-effort")
	step := func(n int) string { return turnTestKey(fmt.Sprint(n)) }
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	first, reused, err := db.resolveBPSRoundIdentity(t.Context(), key, step(1), 100, 2, clock)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, BPSRoundIdentity{0, 1, 100, 2}, first)
	// Attachment preparation or allocation alone does not start the timer.
	now = now.Add(48 * time.Hour)
	unsent, _, err := db.resolveBPSRoundIdentity(t.Context(), key, step(2), 100, 2, clock)
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{0, 2, 100, 2}, unsent)
	started, err := db.touchBPSRoundIdentity(t.Context(), key, 0, now)
	require.NoError(t, err)
	now = now.Add(2*time.Hour - time.Millisecond)
	before, _, err := db.resolveBPSRoundIdentity(t.Context(), key, step(3), 2, 72, clock)
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{0, 3, 100, 2}, before, "settings do not rewrite active batches")
	active, err := db.touchBPSRoundIdentity(t.Context(), key, 0, now)
	require.NoError(t, err)
	require.Equal(t, started.StartedAtMS, active.StartedAtMS)
	require.Equal(t, now.UnixMilli(), active.LastSentAtMS)
	_, _, err = db.resolveBPSRoundIdentity(t.Context(), otherKey, step(1), 100, 72, clock)
	require.NoError(t, err)
	_, err = db.touchBPSRoundIdentity(t.Context(), otherKey, 0, now)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	now = now.Add(time.Millisecond)
	retry, reused, err := db.resolveBPSRoundIdentity(t.Context(), key, step(1), 2, 72, clock)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, first, retry)
	next, reused, err := db.resolveBPSRoundIdentity(t.Context(), key, step(4), 2, 72, clock)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, BPSRoundIdentity{1, 1, 2, 72}, next, "first new call at the fixed deadline rotates despite recent traffic")
	var batches int
	require.NoError(t, db.conn.QueryRow(`SELECT COUNT(*) FROM bps_round_batches WHERE account_key=$1`, key).Scan(&batches))
	require.Equal(t, 1, batches, "new batch remains unsent")
	other, _, err := db.resolveBPSRoundIdentity(t.Context(), otherKey, step(2), 100, 1, clock)
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{0, 2, 100, 72}, other)
	_, err = db.touchBPSRoundIdentity(t.Context(), key, 1, now)
	require.NoError(t, err)
	_, err = db.touchBPSRoundIdentity(t.Context(), key, 0, now.Add(24*time.Hour))
	require.NoError(t, err)
	retry, reused, err = db.resolveBPSRoundIdentity(t.Context(), key, step(1), 2, 72, clock)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, first, retry, "late old sends never alter replay assignments")
	_, _, err = db.resolveBPSRoundIdentity(t.Context(), key, step(5), 2, 72, clock)
	require.NoError(t, err)
	byLimit, _, err := db.resolveBPSRoundIdentity(t.Context(), key, step(6), 2, 72, clock)
	require.NoError(t, err)
	require.Equal(t, BPSRoundIdentity{2, 1, 2, 72}, byLimit, "call limit still rotates before expiry")
	for _, hours := range []int{0, -1, 8761} {
		_, _, err = db.ResolveBPSRoundIdentity(t.Context(), key, step(7), 100, hours)
		require.Error(t, err)
		require.Equal(t, 24, NormalizeBPSRoundTaskLifetimeHours(hours))
	}
	for _, hours := range []int{1, 8760} {
		id, _, err := db.ResolveBPSRoundIdentity(t.Context(), turnTestKey(fmt.Sprint(hours)), step(7), 100, hours)
		require.NoError(t, err)
		require.Equal(t, hours, id.LifetimeHours)
	}
}

func TestBPSRoundIdentityPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	checkBPSRoundConcurrentInstances(t, "postgres", dsn)
}

func TestBPSRoundIdentityMigratesExistingCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-rounds.db")
	db, err := newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name())))
	step := fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-step")))
	old, _, err := db.ResolveBPSRoundIdentity(t.Context(), key, step, 100, 24)
	require.NoError(t, err)
	// Represent the schema deployed before the batch-lifetime table existed.
	_, err = db.conn.Exec(`DROP TABLE bps_round_batches`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	retry, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, step, 100, 24)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, old, retry)
	startedAtMS, err := db.TouchBPSRoundIdentity(t.Context(), key, retry.Generation)
	require.NoError(t, err)
	require.Positive(t, startedAtMS.LastSentAtMS)
}

func checkBPSRoundConcurrentInstances(t *testing.T, driver, dsn string) {
	t.Helper()
	first, err := newBPSTestDB(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := newBPSTestDB(driver, dsn)
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
			value, reused, err := db.ResolveBPSRoundIdentity(t.Context(), key, stepKey, 3, 24)
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
			require.True(t, positions[BPSRoundIdentity{generation, iteration, 3, 24}])
		}
	}
	// Concurrent activity updates preserve the first send and the latest send.
	type startResult struct {
		started BPSRoundBatchActivity
		err     error
	}
	starts := make(chan startResult, 2)
	for i, db := range []*DB{first, second} {
		go func() {
			started, err := db.touchBPSRoundIdentity(t.Context(), key, 3, time.Now().Add(time.Duration(i)*time.Second))
			starts <- startResult{started, err}
		}()
	}
	a, b := <-starts, <-starts
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	require.Positive(t, a.started.LastSentAtMS)
	require.Equal(t, a.started.StartedAtMS, b.started.StartedAtMS)
	latest := max(a.started.LastSentAtMS, b.started.LastSentAtMS)
	var persisted int64
	require.NoError(t, first.conn.QueryRow(`SELECT last_sent_at_unix_ms FROM bps_round_batches WHERE account_key=$1 AND generation=3`, key).Scan(&persisted))
	require.Equal(t, latest, persisted)
	older, err := second.touchBPSRoundIdentity(t.Context(), key, 3, time.UnixMilli(latest-1000))
	require.NoError(t, err)
	require.Equal(t, latest, older.LastSentAtMS, "out-of-order sends cannot move observed activity backwards")
}

func TestBPSRoundIdentityMigratesFirstSendTimer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first-send-timer.db")
	db, err := newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name())))
	step := fmt.Sprintf("%x", sha256.Sum256([]byte("step")))
	_, _, err = db.ResolveBPSRoundIdentity(t.Context(), key, step, 100, 24)
	require.NoError(t, err)
	_, err = db.conn.Exec(`ALTER TABLE bps_round_batches DROP COLUMN last_sent_at_unix_ms`)
	require.NoError(t, err)
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	_, err = db.conn.Exec(`INSERT INTO bps_round_batches(account_key,generation,started_at_unix_ms) VALUES($1,0,$2)`, key, now.UnixMilli())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = newBPSTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	updated, err := db.touchBPSRoundIdentity(t.Context(), key, 0, now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, now.UnixMilli(), updated.StartedAtMS)
	require.Equal(t, now.Add(time.Hour).UnixMilli(), updated.LastSentAtMS)
}
