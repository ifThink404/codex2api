package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The representative event is the latest original request in the query window.
// Counts and time bounds cover every matching event, before group pagination.
type ServiceErrorGroup struct {
	Key       string    `json:"key"`
	Count     int64     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

func serviceErrorGroupKey(event ServiceErrorEvent) string {
	caller := "scope:" + event.ScopeHash
	if event.NewAPIIdentityVerified && event.NewAPIUserID != "" {
		caller = "newapi:" + event.NewAPIUserID
	} else if event.ScopeHash == "" {
		caller = "thread:" + event.ThreadID + ":root:" + event.RootFingerprint
	}
	message := event.Message
	for _, id := range []string{event.NewAPIRequestID, event.RequestID} {
		if id != "" {
			message = strings.ReplaceAll(message, id, "[request-id]")
		}
	}
	reasons := append([]string{}, event.CandidateRejections...)
	slices.Sort(reasons)
	reasons = slices.Compact(reasons)
	failoverReason := ""
	if event.AccountFailover != nil {
		failoverReason = event.AccountFailover.Reason
	}
	// Names, request IDs, durations and window IDs do not split repeat failures.
	// Verified users and different diagnostic causes must remain separate.
	parts := []any{"service-error-v1", event.APIKeyID, caller, event.NewAPIIdentityVerified,
		event.Method, event.Endpoint, event.Transport, event.Model, event.StatusCode,
		event.Stage, event.Code, event.ErrorType, message, event.RequestType,
		event.ThreadSource, event.RequestKind, event.SubagentKind, reasons, failoverReason}
	raw, _ := json.Marshal(parts)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func ValidateServiceErrorGroupKey(value string) bool {
	if value == "" {
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func ValidateServiceErrorViewCursor(value string, grouped bool) bool {
	cursor, err := decodeServiceErrorCursor(value)
	return err == nil && (value == "" || cursor.Grouped == grouped)
}

func (db *DB) ensureServiceErrorGroupingSchema(ctx context.Context) error {
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "service_error_events", "group_key", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	} else if _, err := db.conn.ExecContext(ctx, `ALTER TABLE service_error_events ADD COLUMN IF NOT EXISTS group_key TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_service_errors_group_time ON service_error_events (group_key, created_at DESC, id DESC)`)
	return err
}

// Upgrade existing logs in bounded batches outside the startup/query deadlines.
// Blank keys remain individual rows until backfilled; no event is discarded.
func (db *DB) backfillServiceErrorGroups(ctx context.Context) error {
	for {
		count, err := db.backfillServiceErrorGroupBatch(ctx)
		if err != nil || count == 0 {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (db *DB) backfillServiceErrorGroupBatch(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := db.conn.QueryContext(ctx, `SELECT id, payload FROM service_error_events WHERE group_key = '' ORDER BY created_at DESC, id DESC LIMIT 128`)
	if err != nil {
		return 0, err
	}
	type update struct{ id, key string }
	updates := make([]update, 0, 128)
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		var event ServiceErrorEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			rows.Close()
			return 0, fmt.Errorf("decode service error for grouping: %w", err)
		}
		updates = append(updates, update{id, serviceErrorGroupKey(normalizeServiceError(event))})
	}
	readErr := rows.Err()
	rows.Close() // SQLite may have one connection; close before starting writes.
	if readErr != nil || len(updates) == 0 {
		return 0, readErr
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statement, err := tx.PrepareContext(ctx, `UPDATE service_error_events SET group_key = $1 WHERE id = $2 AND group_key = ''`)
		if err != nil {
			return err
		}
		defer statement.Close()
		for _, item := range updates {
			if _, err := statement.ExecContext(ctx, item.key, item.id); err != nil {
				return err
			}
		}
		return nil
	})
	return len(updates), err
}

func (db *DB) listServiceErrorGroups(ctx context.Context, filter ServiceErrorFilter, cursor serviceErrorCursor, page ServiceErrorPage, where string, args []any) (ServiceErrorPage, error) {
	// Rank only indexed metadata; fetch JSON payloads for the selected page only.
	partition := `PARTITION BY COALESCE(NULLIF(group_key, ''), id)`
	query := `WITH ranked AS (SELECT id, created_at, group_key,
		COUNT(*) OVER (` + partition + `) AS occurrences,
		MIN(created_at) OVER (` + partition + `) AS first_seen,
		ROW_NUMBER() OVER (` + partition + ` ORDER BY created_at DESC, id DESC) AS row_rank
		FROM service_error_events WHERE ` + where + `)
		SELECT events.payload, ranked.group_key, ranked.occurrences, ranked.first_seen, ranked.created_at
		FROM ranked JOIN service_error_events events ON events.id = ranked.id WHERE ranked.row_rank = 1`
	if filter.Cursor != "" {
		args = append(args, cursor.CreatedAt, cursor.ID)
		query += fmt.Sprintf(" AND (ranked.created_at < $%d OR (ranked.created_at = $%d AND ranked.id < $%d))", len(args)-1, len(args)-1, len(args))
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" ORDER BY ranked.created_at DESC, ranked.id DESC LIMIT $%d", len(args))
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var event ServiceErrorEvent
		var payload, key string
		var count, first, last int64
		if err := rows.Scan(&payload, &key, &count, &first, &last); err != nil {
			return page, err
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return page, err
		}
		if key != "" {
			event.Group = &ServiceErrorGroup{Key: key, Count: count, FirstSeen: time.UnixMilli(first).UTC(), LastSeen: time.UnixMilli(last).UTC()}
		}
		page.Items = append(page.Items, event)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		payload, _ := json.Marshal(serviceErrorCursor{CreatedAt: last.CreatedAt.UnixMilli(), ID: last.ID, Grouped: true})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(payload)
	}
	return page, rows.Err()
}
