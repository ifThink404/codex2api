package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// BPS identity retention. The identity tables keep conversation state (task
// and turn identities, iterations, task affinities), so they are not logs:
// rows are only pruned once untouched for longer than every configured
// round/turn task lifetime and at least BPSIdentityMinRetention. Every write
// stamps updated_at, and a read refreshes it when older than
// bpsIdentityTouchAge, so an identity still in use is never pruned.

const (
	BPSIdentityMinRetention = 30 * 24 * time.Hour
	bpsIdentityTouchAge     = 24 * time.Hour
	bpsIdentityPurgeBatch   = 5000
)

// bpsIdentityTables lists each identity table with its primary key columns.
var bpsIdentityTables = []struct {
	name string
	key  []string
}{
	{"codex_identity_uuid7_values", []string{"identity_key"}},
	{"bps_word_turns", []string{"turn_key"}},
	{"bps_word_steps", []string{"turn_key", "step_key"}},
	{"bps_task_affinities", []string{"task_key"}},
	{"bps_round_tasks", []string{"account_key"}},
	{"bps_round_steps", []string{"account_key", "step_key"}},
	{"bps_round_batches", []string{"account_key", "generation"}},
	{"bps_turn_tasks", []string{"account_key"}},
	{"bps_turn_steps", []string{"account_key", "step_key"}},
	{"bps_turn_batches", []string{"account_key", "generation"}},
}

// migrateBPSIdentityUpdatedAt adds updated_at (Unix seconds) and its index to
// every identity table and, once, backfills existing rows with now so they
// get the full retention from this deploy.
func (db *DB) migrateBPSIdentityUpdatedAt(ctx context.Context) error {
	for _, table := range bpsIdentityTables {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, table.name, "updated_at", "BIGINT NOT NULL DEFAULT 0"); err != nil {
				return err
			}
		} else if _, err := db.conn.ExecContext(ctx, "ALTER TABLE "+table.name+" ADD COLUMN IF NOT EXISTS updated_at BIGINT NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if _, err := db.conn.ExecContext(ctx, fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_updated_at ON %s(updated_at)", table.name, table.name)); err != nil {
			return err
		}
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var done bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM transport_plugin_migrations WHERE plugin='bps' AND name='identity_updated_at_backfill')`).Scan(&done); err != nil || done {
			return err
		}
		now := time.Now().Unix()
		for _, table := range bpsIdentityTables {
			if _, err := tx.ExecContext(ctx, "UPDATE "+table.name+" SET updated_at=$1 WHERE updated_at=0", now); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO transport_plugin_migrations(plugin,name) VALUES('bps','identity_updated_at_backfill')`)
		return err
	})
}

// touchBPSIdentity refreshes updated_at of a row just read when it is older
// than bpsIdentityTouchAge (at most one write per row a day). statement's
// last placeholder is the new timestamp, after keys. Failures are ignored:
// the row simply keeps its older stamp.
func (db *DB) touchBPSIdentity(ctx context.Context, updatedAt int64, statement string, keys ...any) {
	now := time.Now()
	if now.Sub(time.Unix(updatedAt, 0)) < bpsIdentityTouchAge {
		return
	}
	_ = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, statement, append(keys, now.Unix())...)
		return err
	})
}

// PruneBPSIdentity deletes identity rows untouched since cutoff, in batches.
func (db *DB) PruneBPSIdentity(ctx context.Context, cutoff time.Time) (int64, error) {
	if db == nil || db.conn == nil {
		return 0, nil
	}
	var total int64
	for _, table := range bpsIdentityTables {
		key := strings.Join(table.key, ",")
		match := key
		if len(table.key) > 1 {
			match = "(" + key + ")"
		}
		statement := fmt.Sprintf("DELETE FROM %s WHERE %s IN (SELECT %s FROM %s WHERE updated_at < $1 LIMIT $2)", table.name, match, key, table.name)
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			var affected int64
			err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
				result, err := tx.ExecContext(ctx, statement, cutoff.Unix(), bpsIdentityPurgeBatch)
				if err != nil {
					return err
				}
				affected, err = result.RowsAffected()
				return err
			})
			if err != nil {
				return total, fmt.Errorf("prune %s: %w", table.name, err)
			}
			total += affected
			if affected < bpsIdentityPurgeBatch {
				break
			}
		}
	}
	return total, nil
}
