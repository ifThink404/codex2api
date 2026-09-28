package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Lightweight metering keeps the numbers that billing windows and usage
// statistics need when usage_log_mode drops a request's detail row. Each event
// lives in exactly one table (usage_logs or usage_metering), so switching modes
// never counts a request twice. Event timestamps are kept instead of coarse
// buckets so sliding API-key and account windows stay exact.
func (db *DB) SetUsageMeteringEnabled(enabled bool) {
	if db != nil {
		db.usageMeteringDisabled.Store(!enabled)
	}
}

// GetUsageMeteringEnabled reports the current in-process switch (default on).
func (db *DB) GetUsageMeteringEnabled() bool {
	return db != nil && !db.usageMeteringDisabled.Load()
}

// SaveUsageMeteringEnabled persists the switch. It is stored outside the
// monolithic system settings upsert so the flag can be written on its own.
func (db *DB) SaveUsageMeteringEnabled(ctx context.Context, enabled bool) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database is not initialized")
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id, usage_metering_enabled) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET usage_metering_enabled = EXCLUDED.usage_metering_enabled`, enabled)
	return err
}

// usageMeteringColumns are the columns shared by usage_logs and usage_metering
// and exposed through usage_metered_events. Statistics queries that read the
// view may only reference these columns (plus id and created_at).
const usageMeteringColumns = `account_id, credential_generation, channel,
	endpoint, inbound_endpoint, model, effective_model,
	prompt_tokens, completion_tokens, total_tokens, input_tokens, output_tokens,
	reasoning_tokens, cached_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
	image_input_tokens, image_output_tokens, cached_image_input_tokens,
	status_code, duration_ms, first_token_ms, api_key_id, api_key_name, api_key_masked,
	account_billed, user_billed, is_retry_attempt, attempt_index, internal_reason,
	stream, compact, image_count, reasoning_effort,
	service_tier, requested_service_tier, actual_service_tier, billing_service_tier`

const usageMeteringColumnCount = 38

func (db *DB) ensureUsageMetering(ctx context.Context) error {
	if db.isSQLite() {
		columns, err := db.sqliteTableColumns(ctx, "system_settings")
		if err != nil {
			return err
		}
		if _, ok := columns["usage_metering_enabled"]; !ok {
			if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN usage_metering_enabled INTEGER NOT NULL DEFAULT 1`); err != nil {
				return err
			}
		}
	} else if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_metering_enabled BOOLEAN NOT NULL DEFAULT TRUE`); err != nil {
		return err
	}
	idType, timestampType, boolType := "SERIAL PRIMARY KEY", "TIMESTAMPTZ", "BOOLEAN"
	if db.isSQLite() {
		idType, timestampType, boolType = "INTEGER PRIMARY KEY AUTOINCREMENT", "TIMESTAMP", "INTEGER"
	}
	// No request bodies, diagnostics, IPs, UAs, traces or error messages. Only
	// three secondary indexes, serving exact time, key and account windows.
	// Column types mirror usage_logs so PostgreSQL can push window predicates
	// through the UNION ALL view into both indexed tables.
	statements := []string{
		`CREATE TABLE IF NOT EXISTS usage_metering (
			id ` + idType + `, created_at ` + timestampType + ` NOT NULL DEFAULT CURRENT_TIMESTAMP,
			account_id INTEGER NOT NULL DEFAULT 0, credential_generation BIGINT NOT NULL DEFAULT 0,
			channel VARCHAR(16) NOT NULL DEFAULT '',
			endpoint VARCHAR(100) NOT NULL DEFAULT '', inbound_endpoint VARCHAR(100) NOT NULL DEFAULT '',
			model VARCHAR(100) NOT NULL DEFAULT '', effective_model VARCHAR(100) NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0, cache_write_5m_tokens INTEGER NOT NULL DEFAULT 0, cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0,
			image_input_tokens INTEGER NOT NULL DEFAULT 0, image_output_tokens INTEGER NOT NULL DEFAULT 0, cached_image_input_tokens INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0, first_token_ms INTEGER NOT NULL DEFAULT 0,
			api_key_id INTEGER NOT NULL DEFAULT 0, api_key_name VARCHAR(255) NOT NULL DEFAULT '', api_key_masked VARCHAR(64) NOT NULL DEFAULT '',
			account_billed DOUBLE PRECISION NOT NULL DEFAULT 0, user_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
			is_retry_attempt ` + boolType + ` NOT NULL DEFAULT ` + db.sqlBool(false) + `, attempt_index INTEGER NOT NULL DEFAULT 0,
			internal_reason VARCHAR(64) NOT NULL DEFAULT '',
			stream ` + boolType + ` NOT NULL DEFAULT ` + db.sqlBool(false) + `, compact ` + boolType + ` NOT NULL DEFAULT ` + db.sqlBool(false) + `,
			image_count INTEGER NOT NULL DEFAULT 0,
			reasoning_effort VARCHAR(100) NOT NULL DEFAULT '', service_tier VARCHAR(100) NOT NULL DEFAULT '',
			requested_service_tier VARCHAR(100) NOT NULL DEFAULT '', actual_service_tier VARCHAR(100) NOT NULL DEFAULT '',
			billing_service_tier VARCHAR(100) NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_metering_created ON usage_metering(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_metering_key_created ON usage_metering(api_key_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_metering_account_created ON usage_metering(account_id, created_at)`,
	}
	// Statistics only: raw log browsing, detail and export keep reading
	// usage_logs. Metering ids are negated so they never collide with log ids.
	viewSelect := ` usage_metered_events AS
		SELECT id, created_at, ` + usageMeteringColumns + ` FROM usage_logs
		UNION ALL
		SELECT -id, created_at, ` + usageMeteringColumns + ` FROM usage_metering`
	if db.isSQLite() {
		// SQLite has no CREATE OR REPLACE VIEW; a single process owns the file.
		statements = append(statements, `DROP VIEW IF EXISTS usage_metered_events`, `CREATE VIEW`+viewSelect)
	} else {
		statements = append(statements, `CREATE OR REPLACE VIEW`+viewSelect)
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	var enabled bool
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(usage_metering_enabled, `+db.sqlBool(true)+`) FROM system_settings WHERE id=1`).Scan(&enabled)
	if err == nil {
		db.SetUsageMeteringEnabled(enabled)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func (db *DB) sqlBool(value bool) string {
	switch {
	case db.isSQLite() && value:
		return "1"
	case db.isSQLite():
		return "0"
	case value:
		return "TRUE"
	default:
		return "FALSE"
	}
}

func countMeteredUsageEntries(batch []usageLogEntry) int {
	count := 0
	for i := range batch {
		if batch[i].StoreMetering {
			count++
		}
	}
	return count
}

// insertUsageMeteringBatch writes the lightweight rows of a flush batch in the
// same transaction as the usage log rows, API key charges and rollups.
func (db *DB) insertUsageMeteringBatch(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	const maxRows = 500 // bounded well below PostgreSQL and SQLite parameter limits
	count := countMeteredUsageEntries(batch)
	if count == 0 {
		return nil
	}
	args := make([]any, 0, min(count, maxRows)*usageMeteringColumnCount)
	var values strings.Builder
	rows := 0
	flush := func() error {
		if rows == 0 {
			return nil
		}
		_, err := execer.ExecContext(ctx, `INSERT INTO usage_metering (`+usageMeteringColumns+`) VALUES `+values.String(), args...)
		rows = 0
		args = args[:0]
		values.Reset()
		if err != nil {
			return fmt.Errorf("insert usage metering: %w", err)
		}
		return nil
	}
	for i := range batch {
		e := &batch[i]
		if !e.StoreMetering {
			continue
		}
		if rows > 0 {
			values.WriteByte(',')
		}
		values.WriteByte('(')
		for col := 0; col < usageMeteringColumnCount; col++ {
			if col > 0 {
				values.WriteByte(',')
			}
			values.WriteByte('$')
			values.WriteString(strconv.Itoa(rows*usageMeteringColumnCount + col + 1))
		}
		values.WriteByte(')')
		args = append(args, e.AccountID, e.CredentialGeneration, e.Channel,
			e.Endpoint, e.InboundEndpoint, e.Model, e.EffectiveModel,
			e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.InputTokens, e.OutputTokens,
			e.ReasoningTokens, e.CachedTokens, e.CacheWrite5mTokens, e.CacheWrite1hTokens,
			e.ImageInputTokens, e.ImageOutputTokens, e.CachedImageInputTokens,
			e.StatusCode, e.DurationMs, e.FirstTokenMs, e.APIKeyID, e.APIKeyName, e.APIKeyMasked,
			e.AccountBilled, e.UserBilled, e.IsRetryAttempt, e.AttemptIndex, e.InternalReason,
			e.Stream, e.Compact, e.ImageCount, e.ReasoningEffort,
			e.ServiceTier, e.RequestedServiceTier, e.ActualServiceTier, e.BillingServiceTier)
		rows++
		if rows == maxRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// usageStatsSource picks the table behind the usage statistics page. Its
// dimension filters (search, request ID, email, client and turn-state fields)
// match request-detail columns that only usage_logs has, so a filtered view
// reads detail rows; unfiltered totals include lightweight metering rows.
func usageStatsSource(dim UsageLogFilter) string {
	if dim.HasDimensionFilter() {
		return "usage_logs"
	}
	return "usage_metered_events"
}
