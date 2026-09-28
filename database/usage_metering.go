package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Lightweight metering is independent of request-detail retention. Each event
// lives in exactly one table, so switching modes never counts a request twice.
// Keep event timestamps (rather than coarse buckets) for sliding-window quotas.
func (db *DB) SetUsageMeteringEnabled(enabled bool) {
	if db != nil {
		db.usageMeteringDisabled.Store(!enabled)
	}
}

func (db *DB) GetUsageMeteringEnabled() bool {
	return db != nil && !db.usageMeteringDisabled.Load()
}

const usageMeteringColumns = `account_id, credential_generation, channel,
	endpoint, inbound_endpoint, model, effective_model,
	prompt_tokens, completion_tokens, total_tokens, input_tokens, output_tokens,
	reasoning_tokens, cached_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
	image_input_tokens, image_output_tokens, cached_image_input_tokens,
	status_code, duration_ms, first_token_ms, api_key_id, api_key_name, api_key_masked,
	account_billed, user_billed, is_retry_attempt, attempt_index, internal_reason,
	stream, compact, image_count, reasoning_effort,
	service_tier, requested_service_tier, actual_service_tier, billing_service_tier`

func (db *DB) ensureUsageMetering(ctx context.Context) error {
	if db.isSQLite() {
		columns, err := db.sqliteTableColumns(ctx, "system_settings")
		if err != nil {
			return err
		}
		if _, ok := columns["usage_metering_enabled"]; !ok {
			if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN usage_metering_enabled BOOLEAN NOT NULL DEFAULT true`); err != nil {
				return err
			}
		}
	} else if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_metering_enabled BOOLEAN NOT NULL DEFAULT true`); err != nil {
		return err
	}
	idType := "SERIAL PRIMARY KEY"
	timestampType := "TIMESTAMPTZ"
	if db.isSQLite() {
		idType = "INTEGER PRIMARY KEY AUTOINCREMENT"
		timestampType = "TIMESTAMP"
	}
	// No request bodies, diagnostics, IPs, UAs, traces, or error messages. Only
	// three secondary indexes, serving exact time, key, and account windows.
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_metering (
		id `+idType+`, created_at `+timestampType+` NOT NULL DEFAULT CURRENT_TIMESTAMP,
		account_id INTEGER NOT NULL, credential_generation BIGINT NOT NULL,
		channel VARCHAR(16) NOT NULL,
		endpoint VARCHAR(100) NOT NULL, inbound_endpoint VARCHAR(100) NOT NULL,
		model VARCHAR(100) NOT NULL, effective_model VARCHAR(100) NOT NULL,
		prompt_tokens INTEGER NOT NULL, completion_tokens INTEGER NOT NULL, total_tokens INTEGER NOT NULL,
		input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL,
		cached_tokens INTEGER NOT NULL, cache_write_5m_tokens INTEGER NOT NULL, cache_write_1h_tokens INTEGER NOT NULL,
		image_input_tokens INTEGER NOT NULL, image_output_tokens INTEGER NOT NULL, cached_image_input_tokens INTEGER NOT NULL,
		status_code INTEGER NOT NULL, duration_ms INTEGER NOT NULL, first_token_ms INTEGER NOT NULL,
		api_key_id INTEGER NOT NULL, api_key_name VARCHAR(255) NOT NULL, api_key_masked VARCHAR(64) NOT NULL,
		account_billed DOUBLE PRECISION NOT NULL, user_billed DOUBLE PRECISION NOT NULL,
		is_retry_attempt BOOLEAN NOT NULL, attempt_index INTEGER NOT NULL, internal_reason VARCHAR(64) NOT NULL,
		stream BOOLEAN NOT NULL, compact BOOLEAN NOT NULL, image_count INTEGER NOT NULL,
		reasoning_effort VARCHAR(100) NOT NULL, service_tier VARCHAR(100) NOT NULL,
		requested_service_tier VARCHAR(100) NOT NULL, actual_service_tier VARCHAR(100) NOT NULL,
		billing_service_tier VARCHAR(100) NOT NULL, overloaded_500 BOOLEAN NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_usage_metering_created ON usage_metering(created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_metering_key_created ON usage_metering(api_key_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_metering_account_created ON usage_metering(account_id, created_at)`)
	if err != nil {
		return err
	}
	// Statistics only: raw log browsing/export must continue to use usage_logs.
	// Matching column types allow PostgreSQL to push window predicates through
	// UNION ALL into the indexed base tables. No historical copy/backfill needed.
	viewDDL := "CREATE OR REPLACE VIEW"
	if db.isSQLite() {
		viewDDL = "CREATE VIEW IF NOT EXISTS"
	}
	_, err = db.conn.ExecContext(ctx, viewDDL+` usage_metered_events AS
		SELECT id, created_at, `+usageMeteringColumns+`, error_message FROM usage_logs
		UNION ALL
		SELECT -id, created_at, `+usageMeteringColumns+`,
			CASE WHEN overloaded_500 THEN 'server_is_overloaded' ELSE '' END AS error_message
		FROM usage_metering`)
	if err != nil {
		return err
	}
	var enabled bool
	err = db.conn.QueryRowContext(ctx, `SELECT usage_metering_enabled FROM system_settings WHERE id=1`).Scan(&enabled)
	if err == nil {
		db.SetUsageMeteringEnabled(enabled)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// Only the presence of an error and the overload classification are needed by
// accounting/session hooks. Do not retain a potentially huge response body in
// the asynchronous queue when detail logging is disabled.
func compactUsageError(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "server_is_overloaded" || strings.HasPrefix(trimmed, "server_is_overloaded · ") {
		return "server_is_overloaded"
	}
	if message != "" {
		return "error"
	}
	return ""
}

func (db *DB) insertUsageMeteringBatch(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	const columns = 39  // usageMeteringColumns plus overloaded_500
	const maxRows = 500 // bounded well below PostgreSQL and SQLite parameter limits
	count := 0
	for i := range batch {
		if batch[i].StoreMetering {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	args := make([]any, 0, min(count, maxRows)*columns)
	var values strings.Builder
	values.Grow(min(count, maxRows) * columns * 7)
	rows := 0
	flush := func() error {
		if rows == 0 {
			return nil
		}
		_, err := execer.ExecContext(ctx, `INSERT INTO usage_metering (`+usageMeteringColumns+`, overloaded_500) VALUES `+values.String(), args...)
		rows = 0
		args = args[:0]
		values.Reset()
		return err
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
		for col := 0; col < columns; col++ {
			if col > 0 {
				values.WriteByte(',')
			}
			values.WriteByte('$')
			values.WriteString(strconv.Itoa(rows*columns + col + 1))
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
			e.ServiceTier, e.RequestedServiceTier, e.ActualServiceTier, e.BillingServiceTier,
			e.StatusCode == 500 && compactUsageError(e.ErrorMessage) == "server_is_overloaded")
		rows++
		if rows == maxRows {
			if err := flush(); err != nil {
				return fmt.Errorf("insert usage metering: %w", err)
			}
		}
	}
	return flush()
}
