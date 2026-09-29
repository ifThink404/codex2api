package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openBPSIdentityTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := newBPSTestDB("sqlite", filepath.Join(t.TempDir(), "identity.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func bpsKey(c string) string { return strings.Repeat(c, 64) }

func identityUpdatedAt(t *testing.T, db *DB, table, where string, args ...any) int64 {
	t.Helper()
	var updatedAt int64
	require.NoError(t, db.conn.QueryRow("SELECT updated_at FROM "+table+" WHERE "+where, args...).Scan(&updatedAt))
	return updatedAt
}

func TestBPSIdentityWritesStampUpdatedAt(t *testing.T) {
	db := openBPSIdentityTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	_, err := db.ResolveCodexIdentityUUIDv7(ctx, bpsKey("a"), strings.Repeat("ab", 32))
	require.NoError(t, err)
	_, _, err = db.ResolveBPSWordIteration(ctx, bpsKey("b"), bpsKey("c"))
	require.NoError(t, err)
	_, _, err = db.ResolveBPSRoundIdentity(ctx, bpsKey("d"), bpsKey("e"), 10, 24)
	require.NoError(t, err)
	_, err = db.TouchBPSRoundIdentity(ctx, bpsKey("d"), 0)
	require.NoError(t, err)
	_, _, err = db.ResolveBPSTurnTaskIdentity(ctx, bpsKey("f"), bpsKey("1"), 24)
	require.NoError(t, err)
	_, err = db.TouchBPSTurnTaskIdentity(ctx, bpsKey("f"), 0)
	require.NoError(t, err)
	_, err = db.UpdateBPSTaskAffinity(ctx, bpsKey("2"), 0, 7)
	require.NoError(t, err)
	for _, table := range bpsIdentityTables {
		var stale int
		require.NoError(t, db.conn.QueryRow("SELECT COUNT(*) FROM "+table.name+" WHERE updated_at < $1", now-5).Scan(&stale))
		var rows int
		require.NoError(t, db.conn.QueryRow("SELECT COUNT(*) FROM "+table.name).Scan(&rows))
		require.NotZero(t, rows, table.name)
		require.Zero(t, stale, "%s rows must carry a fresh updated_at", table.name)
	}
}

func TestBPSIdentityReadsRefreshStaleRowsOnly(t *testing.T) {
	db := openBPSIdentityTestDB(t)
	ctx := context.Background()
	_, _, err := db.ResolveBPSRoundIdentity(ctx, bpsKey("a"), bpsKey("b"), 10, 24)
	require.NoError(t, err)
	hourAgo, daysAgo := time.Now().Add(-time.Hour).Unix(), time.Now().Add(-48*time.Hour).Unix()

	_, err = db.conn.Exec("UPDATE bps_round_steps SET updated_at=$1", hourAgo)
	require.NoError(t, err)
	_, reused, err := db.ResolveBPSRoundIdentity(ctx, bpsKey("a"), bpsKey("b"), 10, 24)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, hourAgo, identityUpdatedAt(t, db, "bps_round_steps", "account_key=$1", bpsKey("a")), "a fresh row is not rewritten on read")

	_, err = db.conn.Exec("UPDATE bps_round_steps SET updated_at=$1", daysAgo)
	require.NoError(t, err)
	_, err = db.conn.Exec("UPDATE bps_round_tasks SET updated_at=$1", daysAgo)
	require.NoError(t, err)
	_, _, err = db.ResolveBPSRoundIdentity(ctx, bpsKey("a"), bpsKey("b"), 10, 24)
	require.NoError(t, err)
	require.Greater(t, identityUpdatedAt(t, db, "bps_round_steps", "account_key=$1", bpsKey("a")), daysAgo, "a stale row in use is refreshed")
	require.Greater(t, identityUpdatedAt(t, db, "bps_round_tasks", "account_key=$1", bpsKey("a")), daysAgo, "together with its task")

	_, err = db.ResolveCodexIdentityUUIDv7(ctx, bpsKey("4"), strings.Repeat("cd", 32))
	require.NoError(t, err)
	_, err = db.conn.Exec("UPDATE codex_identity_uuid7_values SET updated_at=$1", daysAgo)
	require.NoError(t, err)
	_, err = db.ResolveCodexIdentityUUIDv7(ctx, bpsKey("4"), strings.Repeat("cd", 32))
	require.NoError(t, err)
	require.Greater(t, identityUpdatedAt(t, db, "codex_identity_uuid7_values", "identity_key=$1", bpsKey("4")), daysAgo)
}

func TestBPSIdentityPruneAndBackfill(t *testing.T) {
	db := openBPSIdentityTestDB(t)
	ctx := context.Background()
	for _, c := range []string{"a", "b"} {
		_, _, err := db.ResolveBPSTurnTaskIdentity(ctx, bpsKey(c), bpsKey("3"), 24)
		require.NoError(t, err)
		_, err = db.ResolveCodexIdentityUUIDv7(ctx, bpsKey(c), strings.Repeat("ef", 32))
		require.NoError(t, err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	for _, table := range []string{"bps_turn_tasks", "bps_turn_steps", "codex_identity_uuid7_values"} {
		column := "account_key"
		if table == "codex_identity_uuid7_values" {
			column = "identity_key"
		}
		_, err := db.conn.Exec("UPDATE "+table+" SET updated_at=$1 WHERE "+column+"=$2", old, bpsKey("a"))
		require.NoError(t, err)
	}
	deleted, err := db.PruneBPSIdentity(ctx, time.Now().Add(-BPSIdentityMinRetention))
	require.NoError(t, err)
	require.EqualValues(t, 3, deleted, "only the rows untouched for 40 days")
	var left int
	require.NoError(t, db.conn.QueryRow("SELECT COUNT(*) FROM bps_turn_steps").Scan(&left))
	require.Equal(t, 1, left)

	// The one-time backfill stamps rows written before the column existed.
	_, err = db.conn.Exec("UPDATE bps_turn_tasks SET updated_at=0")
	require.NoError(t, err)
	_, err = db.conn.Exec("DELETE FROM transport_plugin_migrations WHERE name='identity_updated_at_backfill'")
	require.NoError(t, err)
	require.NoError(t, db.MigrateBPSPlugin(ctx))
	require.Greater(t, identityUpdatedAt(t, db, "bps_turn_tasks", "account_key=$1", bpsKey("b")), time.Now().Add(-time.Minute).Unix())
	require.NoError(t, db.MigrateBPSPlugin(ctx), "the migration is idempotent")
}
