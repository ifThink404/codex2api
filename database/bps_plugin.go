package database

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

// Tables owned by the BPS transport plugin. The plugin runs MigrateBPSPlugin
// from its Migrate hook; core migrations never reference these tables. Table
// names match the fj-server implementation so existing data carries over.

var ErrCodexIdentityAliasCollision = errors.New("codex outbound identity alias collision")

// ValidSessionOperationKey reports whether key is a lowercase hex SHA-256
// digest (the only key shape the identity tables accept).
func ValidSessionOperationKey(key string) bool {
	decoded, err := hex.DecodeString(key)
	return len(key) == 64 && len(decoded) == 32 && err == nil && key == strings.ToLower(key)
}

// BPS attachment upload concurrency limits (plugin config bounds).
const (
	DefaultBPSAttachmentAccountConcurrency  = 15
	MaxBPSAttachmentAccountConcurrency      = 1024
	DefaultBPSAttachmentRequestConcurrency  = 15
	MaxBPSAttachmentRequestConcurrency      = 64
	DefaultBPSAttachmentInstanceConcurrency = 64
	MaxBPSAttachmentInstanceConcurrency     = 1024
)

func NormalizeBPSAttachmentRequestConcurrency(value int) int {
	if value < 1 || value > MaxBPSAttachmentRequestConcurrency {
		return DefaultBPSAttachmentRequestConcurrency
	}
	return value
}

func NormalizeBPSAttachmentInstanceConcurrency(value int) int {
	if value < 1 || value > MaxBPSAttachmentInstanceConcurrency {
		return DefaultBPSAttachmentInstanceConcurrency
	}
	return value
}

func NormalizeBPSAttachmentAccountConcurrency(value int) int {
	if value < 1 || value > MaxBPSAttachmentAccountConcurrency {
		return DefaultBPSAttachmentAccountConcurrency
	}
	return value
}

// MigrateBPSPlugin creates the BPS identity tables and runs the plugin's
// one-time data migrations.
func (db *DB) MigrateBPSPlugin(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return nil
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS codex_identity_uuid7_values (identity_key TEXT PRIMARY KEY, value TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS bps_word_turns (turn_key TEXT PRIMARY KEY, iteration BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS bps_word_steps (turn_key TEXT NOT NULL, step_key TEXT NOT NULL, iteration BIGINT NOT NULL, PRIMARY KEY(turn_key,step_key))`,
		`CREATE TABLE IF NOT EXISTS bps_task_affinities (task_key TEXT PRIMARY KEY, account_id BIGINT NOT NULL, revision BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS bps_round_tasks (account_key TEXT PRIMARY KEY, generation BIGINT NOT NULL, iteration BIGINT NOT NULL, round_limit INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS bps_round_steps (account_key TEXT NOT NULL, step_key TEXT NOT NULL, generation BIGINT NOT NULL, iteration BIGINT NOT NULL, round_limit INTEGER NOT NULL, PRIMARY KEY(account_key,step_key))`,
		`CREATE TABLE IF NOT EXISTS bps_round_batches (account_key TEXT NOT NULL, generation BIGINT NOT NULL, started_at_unix_ms BIGINT NOT NULL, last_sent_at_unix_ms BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(account_key,generation))`,
		`CREATE TABLE IF NOT EXISTS bps_turn_tasks (account_key TEXT PRIMARY KEY, generation BIGINT NOT NULL, lifetime_hours INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS bps_turn_steps (account_key TEXT NOT NULL, step_key TEXT NOT NULL, generation BIGINT NOT NULL, lifetime_hours INTEGER NOT NULL, PRIMARY KEY(account_key,step_key))`,
		`CREATE TABLE IF NOT EXISTS bps_turn_batches (account_key TEXT NOT NULL, generation BIGINT NOT NULL, started_at_unix_ms BIGINT NOT NULL, last_sent_at_unix_ms BIGINT NOT NULL, PRIMARY KEY(account_key,generation))`,
		`CREATE TABLE IF NOT EXISTS transport_plugin_migrations (plugin TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(plugin,name))`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, column := range []struct{ table, name, definition string }{
		{"bps_round_batches", "last_sent_at_unix_ms", "BIGINT NOT NULL DEFAULT 0"},
		{"bps_round_tasks", "lifetime_hours", "INTEGER NOT NULL DEFAULT 24"},
		{"bps_round_steps", "lifetime_hours", "INTEGER NOT NULL DEFAULT 24"},
	} {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, column.table, column.name, column.definition); err != nil {
				return err
			}
		} else if _, err := db.conn.ExecContext(ctx, "ALTER TABLE "+column.table+" ADD COLUMN IF NOT EXISTS "+column.name+" "+column.definition); err != nil {
			return err
		}
	}
	if err := db.migrateBPSIdentityUpdatedAt(ctx); err != nil {
		return err
	}
	if err := db.migrateBPSPolicyBlocks(ctx); err != nil {
		return err
	}
	return db.migrateLegacyBPSSwitch(ctx)
}

// migrateLegacyBPSSwitch clears codex_bps_enabled=false once. Before the
// plugin, false only meant "not opted in"; as the plugin's per-account
// override it would mean "forced off" and silently block group enablement.
// Changed accounts get an outbox event so every replica reloads them.
func (db *DB) migrateLegacyBPSSwitch(ctx context.Context) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var done bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM transport_plugin_migrations WHERE plugin='bps' AND name='legacy_false_switch')`).Scan(&done); err != nil || done {
			return err
		}
		var rows *sql.Rows
		var err error
		if db.isSQLite() {
			rows, err = tx.QueryContext(ctx, `SELECT id FROM accounts WHERE json_type(credentials,'$.codex_bps_enabled') IN ('false','text') AND LOWER(CAST(json_extract(credentials,'$.codex_bps_enabled') AS TEXT)) IN ('0','false')`)
		} else {
			rows, err = tx.QueryContext(ctx, `SELECT id FROM accounts WHERE LOWER(credentials->>'codex_bps_enabled') = 'false'`)
		}
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			query := `UPDATE accounts SET credentials = credentials - 'codex_bps_enabled' WHERE id = $1`
			if db.isSQLite() {
				query = `UPDATE accounts SET credentials = json_remove(credentials,'$.codex_bps_enabled') WHERE id = $1`
			}
			if _, err := tx.ExecContext(ctx, query, id); err != nil {
				return err
			}
			if err := insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityAccount, id, "updated"); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO transport_plugin_migrations(plugin,name) VALUES('bps','legacy_false_switch')`)
		return err
	})
}
