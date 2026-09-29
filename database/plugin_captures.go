package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Plugin capture store: sampled, masked request/response snapshots taken
// around transport plugin execution (see proxy/plugins/capture.go). Rows are
// diagnostics only; they join usage_logs by request_id. Retention has two
// windows per plugin: error captures (and the other captures of the same
// attempt) are kept up to PluginCaptureMaxRetention, other captures for a
// shorter, configurable window.

const (
	DefaultPluginCaptureRetention      = 6 * time.Hour
	DefaultPluginCaptureErrorRetention = 12 * time.Hour
	// PluginCaptureMaxRetention caps both windows; nothing is kept longer.
	PluginCaptureMaxRetention      = 12 * time.Hour
	PluginCaptureMinRetention      = time.Hour
	DefaultPluginCapturePurgeBatch = 5000
	// PluginCaptureVacuumThreshold: a purge deleting at least this many rows
	// is followed by VACUUM on PostgreSQL to return the space for reuse.
	PluginCaptureVacuumThreshold = 5000
	// PluginCaptureBodyLimit caps the stored body (bytes). Writers truncate
	// before insert and set Truncated.
	PluginCaptureBodyLimit = 64 * 1024
)

const (
	PluginCaptureDirectionRequest  = "request"
	PluginCaptureDirectionResponse = "response"
	PluginCaptureDirectionError    = "error"
	// PluginCaptureDirectionUpstreamRequest is the request a plugin actually
	// sent upstream (after its own transformation).
	PluginCaptureDirectionUpstreamRequest = "upstream_request"
)

type PluginCapture struct {
	ID        int64     `json:"id"`
	Plugin    string    `json:"plugin"`
	RequestID string    `json:"request_id"`
	AccountID int64     `json:"account_id"`
	Attempt   int       `json:"attempt"`
	Direction string    `json:"direction"`
	Status    int       `json:"status"`
	Headers   string    `json:"headers"`
	Body      string    `json:"body,omitempty"`
	BodyBytes int       `json:"body_bytes"`
	ErrorKind string    `json:"error_kind"`
	Truncated bool      `json:"truncated"`
	CreatedAt time.Time `json:"created_at"`
}

type PluginCaptureFilter struct {
	Plugin    string
	RequestID string
	AccountID *int64
	Status    *int
	Direction string
	Start     time.Time
	End       time.Time
	Page      int
	PageSize  int
}

type PluginCapturePage struct {
	Captures []*PluginCapture `json:"captures"`
	Total    int64            `json:"total"`
}

type PluginCapturePurgeResult struct {
	Deleted     int64 `json:"deleted"`
	Batches     int   `json:"batches"`
	Interrupted bool  `json:"interrupted"`
}

// InsertPluginCaptures writes one batch in a single transaction.
func (db *DB) InsertPluginCaptures(ctx context.Context, captures []PluginCapture) error {
	if db == nil || db.conn == nil || len(captures) == 0 {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO plugin_captures (plugin, request_id, account_id, attempt, direction, status, headers, body, error_kind, truncated, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, c := range captures {
			createdAt := c.CreatedAt
			if createdAt.IsZero() {
				createdAt = time.Now()
			}
			if _, err := stmt.ExecContext(ctx, c.Plugin, clampUsageLogText(c.RequestID, usageLogRequestIDMaxLen), c.AccountID, c.Attempt,
				c.Direction, c.Status, c.Headers, c.Body, clampUsageLogText(c.ErrorKind, usageLogShortTextMaxLen), c.Truncated, db.timeArg(createdAt.UTC())); err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) pluginCaptureWhere(f PluginCaptureFilter) (string, []interface{}) {
	parts := []string{"1=1"}
	args := []interface{}{}
	add := func(clause string, value interface{}) {
		args = append(args, value)
		parts = append(parts, fmt.Sprintf(clause, fmt.Sprintf("$%d", len(args))))
	}
	if f.Plugin != "" {
		add("plugin = %s", f.Plugin)
	}
	if f.RequestID != "" {
		add("request_id = %s", f.RequestID)
	}
	if f.AccountID != nil {
		add("account_id = %s", *f.AccountID)
	}
	if f.Status != nil {
		add("status = %s", *f.Status)
	}
	if f.Direction != "" {
		add("direction = %s", f.Direction)
	}
	if !f.Start.IsZero() {
		add("created_at >= %s", db.timeArg(f.Start.UTC()))
	}
	if !f.End.IsZero() {
		add("created_at <= %s", db.timeArg(f.End.UTC()))
	}
	return strings.Join(parts, " AND "), args
}

// ListPluginCaptures returns one page, newest first. Bodies are omitted from
// list rows (BodyBytes reports their stored length); GetPluginCapture returns
// the full row.
func (db *DB) ListPluginCaptures(ctx context.Context, f PluginCaptureFilter) (*PluginCapturePage, error) {
	if db == nil || db.conn == nil {
		return &PluginCapturePage{}, nil
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	where, args := db.pluginCaptureWhere(f)
	page := &PluginCapturePage{Captures: []*PluginCapture{}}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_captures WHERE `+where, args...).Scan(&page.Total); err != nil {
		return nil, err
	}
	limitIdx := len(args) + 1
	query := fmt.Sprintf(`SELECT id, plugin, request_id, account_id, attempt, direction, status, headers, LENGTH(body), error_kind, truncated, created_at
		FROM plugin_captures WHERE %s ORDER BY id DESC LIMIT $%d OFFSET $%d`, where, limitIdx, limitIdx+1)
	rows, err := db.conn.QueryContext(ctx, query, append(args, f.PageSize, (f.Page-1)*f.PageSize)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c := &PluginCapture{}
		var createdRaw any
		if err := rows.Scan(&c.ID, &c.Plugin, &c.RequestID, &c.AccountID, &c.Attempt, &c.Direction, &c.Status, &c.Headers, &c.BodyBytes, &c.ErrorKind, &c.Truncated, &createdRaw); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = parseDBTimeValue(createdRaw)
		page.Captures = append(page.Captures, c)
	}
	return page, rows.Err()
}

// GetPluginCapture returns (nil, nil) when the row does not exist.
func (db *DB) GetPluginCapture(ctx context.Context, id int64) (*PluginCapture, error) {
	if db == nil || db.conn == nil {
		return nil, nil
	}
	c := &PluginCapture{}
	var createdRaw any
	err := db.conn.QueryRowContext(ctx, `SELECT id, plugin, request_id, account_id, attempt, direction, status, headers, body, error_kind, truncated, created_at
		FROM plugin_captures WHERE id = $1`, id).Scan(&c.ID, &c.Plugin, &c.RequestID, &c.AccountID, &c.Attempt, &c.Direction, &c.Status, &c.Headers, &c.Body, &c.ErrorKind, &c.Truncated, &createdRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.BodyBytes = len(c.Body)
	c.CreatedAt, _ = parseDBTimeValue(createdRaw)
	return c, nil
}

// PurgePluginCaptures deletes rows older than cutoff in batches, releasing the
// write lock between batches (same shape as the prompt log retention purge).
func (db *DB) PurgePluginCaptures(ctx context.Context, cutoff time.Time, batchSize int) (PluginCapturePurgeResult, error) {
	var result PluginCapturePurgeResult
	if db == nil || db.conn == nil || cutoff.IsZero() {
		return result, nil
	}
	if batchSize <= 0 {
		batchSize = DefaultPluginCapturePurgeBatch
	}
	for {
		if ctx.Err() != nil {
			result.Interrupted = true
			return result, nil
		}
		var affected int64
		err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `DELETE FROM plugin_captures WHERE id IN (
				SELECT id FROM plugin_captures WHERE created_at < $1 ORDER BY id LIMIT $2)`, db.timeArg(cutoff.UTC()), batchSize)
			if err != nil {
				return err
			}
			affected, err = res.RowsAffected()
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				result.Interrupted = true
				return result, nil
			}
			return result, err
		}
		result.Batches++
		result.Deleted += affected
		if affected < int64(batchSize) {
			return result, nil
		}
	}
}

// Error captures are rows with an HTTP error status, a classified error kind
// (set by the plugin, e.g. a usage-policy block inside a 200 stream) or an
// execute error: status >= 400 OR error_kind <> '' OR direction = 'error'.

// deletePluginCapturesBatched deletes the rows matching where (with args,
// placeholders $1..$n) in batches, releasing the write lock between batches.
func (db *DB) deletePluginCapturesBatched(ctx context.Context, where string, args []interface{}, batchSize int) (PluginCapturePurgeResult, error) {
	var result PluginCapturePurgeResult
	if db == nil || db.conn == nil {
		return result, nil
	}
	if batchSize <= 0 {
		batchSize = DefaultPluginCapturePurgeBatch
	}
	query := fmt.Sprintf(`DELETE FROM plugin_captures WHERE id IN (
		SELECT id FROM plugin_captures c WHERE %s ORDER BY id LIMIT $%d)`, where, len(args)+1)
	for {
		if ctx.Err() != nil {
			result.Interrupted = true
			return result, nil
		}
		var affected int64
		err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, query, append(append([]interface{}{}, args...), batchSize)...)
			if err != nil {
				return err
			}
			affected, err = res.RowsAffected()
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				result.Interrupted = true
				return result, nil
			}
			return result, err
		}
		result.Batches++
		result.Deleted += affected
		if affected < int64(batchSize) {
			return result, nil
		}
	}
}

// PurgePluginCaptureWindows applies one plugin's two retention windows: every
// row older than errorCutoff goes, and a row older than normalCutoff goes
// unless it is an error capture or belongs to an attempt that has one.
func (db *DB) PurgePluginCaptureWindows(ctx context.Context, plugin string, normalCutoff, errorCutoff time.Time, batchSize int) (PluginCapturePurgeResult, error) {
	total, err := db.deletePluginCapturesBatched(ctx, "c.plugin = $1 AND c.created_at < $2",
		[]interface{}{plugin, db.timeArg(errorCutoff.UTC())}, batchSize)
	if err != nil || total.Interrupted {
		return total, err
	}
	normal, err := db.deletePluginCapturesBatched(ctx, `c.plugin = $1 AND c.created_at < $2 AND NOT (c.status >= 400 OR c.error_kind <> '' OR c.direction = 'error') AND NOT EXISTS (
			SELECT 1 FROM plugin_captures e WHERE e.request_id = c.request_id AND e.request_id <> '' AND e.attempt = c.attempt AND e.plugin = c.plugin
				AND (e.status >= 400 OR e.error_kind <> '' OR e.direction = 'error'))`,
		[]interface{}{plugin, db.timeArg(normalCutoff.UTC())}, batchSize)
	total.Deleted += normal.Deleted
	total.Batches += normal.Batches
	total.Interrupted = normal.Interrupted
	return total, err
}

// Manual capture purge modes.
const (
	PluginCapturePurgeAll        = "all"
	PluginCapturePurgeErrorsOnly = "errors_only"
	PluginCapturePurgeOlderThan  = "older_than"
)

// PurgePluginCapturesByMode deletes one plugin's captures: all of them, only
// error captures, or those older than cutoff (older_than).
func (db *DB) PurgePluginCapturesByMode(ctx context.Context, plugin, mode string, cutoff time.Time) (PluginCapturePurgeResult, error) {
	switch mode {
	case PluginCapturePurgeAll:
		return db.deletePluginCapturesBatched(ctx, "c.plugin = $1", []interface{}{plugin}, 0)
	case PluginCapturePurgeErrorsOnly:
		return db.deletePluginCapturesBatched(ctx, "c.plugin = $1 AND (c.status >= 400 OR c.error_kind <> '' OR c.direction = 'error')", []interface{}{plugin}, 0)
	case PluginCapturePurgeOlderThan:
		if cutoff.IsZero() {
			return PluginCapturePurgeResult{}, errors.New("older_than requires a cutoff")
		}
		return db.deletePluginCapturesBatched(ctx, "c.plugin = $1 AND c.created_at < $2", []interface{}{plugin, db.timeArg(cutoff.UTC())}, 0)
	}
	return PluginCapturePurgeResult{}, fmt.Errorf("unknown purge mode %q", mode)
}

// VacuumPluginCaptures returns freed space to PostgreSQL after a large purge
// (plain VACUUM, never FULL, so it takes no exclusive lock). SQLite: no-op.
func (db *DB) VacuumPluginCaptures(ctx context.Context) error {
	if db == nil || db.conn == nil || db.isSQLite() {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `VACUUM (ANALYZE) plugin_captures`)
	return err
}

// PluginCaptureStats describes one plugin's stored captures.
type PluginCaptureStats struct {
	Rows       int64 `json:"rows"`
	ErrorRows  int64 `json:"error_rows"`
	BodyBytes  int64 `json:"body_bytes"`
	TableBytes int64 `json:"table_bytes"`
}

// PluginCaptureStats counts one plugin's rows and stored payload bytes;
// TableBytes is the whole table's on-disk size (PostgreSQL only, else 0).
func (db *DB) PluginCaptureStats(ctx context.Context, plugin string) (PluginCaptureStats, error) {
	var stats PluginCaptureStats
	if db == nil || db.conn == nil {
		return stats, nil
	}
	err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status >= 400 OR error_kind <> '' OR direction = 'error' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(LENGTH(body) + LENGTH(headers)), 0)
		FROM plugin_captures WHERE plugin = $1`, plugin).Scan(&stats.Rows, &stats.ErrorRows, &stats.BodyBytes)
	if err != nil {
		return stats, err
	}
	if !db.isSQLite() {
		_ = db.conn.QueryRowContext(ctx, `SELECT pg_total_relation_size('plugin_captures')`).Scan(&stats.TableBytes)
	}
	return stats, nil
}
