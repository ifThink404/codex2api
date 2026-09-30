package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	serviceErrorQueueCapacity = 512
	serviceErrorBatchSize     = 64
	serviceErrorRetention     = 7 * 24 * time.Hour
	serviceErrorMaxRows       = 100000
)

// ServiceErrorEvent is a gateway-local rejection or failure: authentication,
// local limits, validation, policy and dispatch errors. Upstream account
// errors stay in the usage/error logs. No request body or credential is kept.
type ServiceErrorEvent struct {
	Group                  *ServiceErrorGroup `json:"group,omitempty"`
	ID                     string             `json:"id"`
	CreatedAt              time.Time          `json:"created_at"`
	RequestID              string             `json:"request_id"`
	NewAPIRequestID        string             `json:"newapi_request_id,omitempty"`
	NewAPIIdentityVerified bool               `json:"newapi_identity_verified"`
	NewAPIUserID           string             `json:"newapi_user_id,omitempty"`
	NewAPIUserName         string             `json:"newapi_user_name,omitempty"`
	StatusCode             int                `json:"status_code"`
	Code                   string             `json:"code"`
	ErrorType              string             `json:"error_type"`
	Message                string             `json:"message"`
	Stage                  string             `json:"stage"`
	Method                 string             `json:"method"`
	Endpoint               string             `json:"endpoint"`
	Transport              string             `json:"transport"`
	Model                  string             `json:"model,omitempty"`
	DurationMs             int64              `json:"duration_ms"`
	APIKeyID               int64              `json:"api_key_id,omitempty"`
	APIKeyName             string             `json:"api_key_name,omitempty"`
	ThreadSource           string             `json:"thread_source,omitempty"`
	RequestKind            string             `json:"request_kind,omitempty"`
	ThreadID               string             `json:"thread_id,omitempty"`
	ClientInfo             map[string]string  `json:"client_info,omitempty"`
	ToolProtocol           json.RawMessage    `json:"tool_protocol,omitempty"`
}

type ServiceErrorFilter struct {
	Grouped   bool
	GroupKey  string
	Start     time.Time
	End       time.Time
	Status    string
	Stage     string
	RequestID string
	Cursor    string
	Limit     int
}

type ServiceErrorSummary struct {
	Groups    int64 `json:"groups"`
	Total     int64 `json:"total"`
	Status429 int64 `json:"status_429"`
	Status4xx int64 `json:"status_4xx"`
	Status5xx int64 `json:"status_5xx"`
}

type ServiceErrorCollectorStats struct {
	Pending       int64  `json:"pending"`
	Written       uint64 `json:"written"`
	Dropped       uint64 `json:"dropped"`
	WriteFailures uint64 `json:"write_failures"`
	Capacity      int    `json:"capacity"`
	RetentionDays int    `json:"retention_days"`
	MaxRows       int    `json:"max_rows"`
}

type ServiceErrorPage struct {
	Grouped    bool                       `json:"grouped"`
	Items      []ServiceErrorEvent        `json:"items"`
	NextCursor string                     `json:"next_cursor,omitempty"`
	Summary    ServiceErrorSummary        `json:"summary"`
	Collector  ServiceErrorCollectorStats `json:"collector"`
}

type serviceErrorJob struct {
	event   ServiceErrorEvent
	payload string
}

// serviceErrorQueue keeps the request hot path off the database: a bounded
// channel drained by a single writer in batches. A full queue drops and counts.
type serviceErrorQueue struct {
	db       *DB
	jobs     chan serviceErrorJob
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.RWMutex
	closed   bool
	pending  atomic.Int64
	written  atomic.Uint64
	dropped  atomic.Uint64
	failed   atomic.Uint64
	lastWarn atomic.Int64
}

func (db *DB) ensureServiceErrorSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS service_error_events (
			id TEXT PRIMARY KEY, created_at BIGINT NOT NULL, status_code INTEGER NOT NULL,
			stage TEXT NOT NULL, request_id TEXT NOT NULL, newapi_request_id TEXT NOT NULL,
			payload TEXT NOT NULL, group_key TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_service_errors_time ON service_error_events (created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_service_errors_status_time ON service_error_events (status_code, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_service_errors_stage_time ON service_error_events (stage, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_service_errors_request ON service_error_events (request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_service_errors_newapi_request ON service_error_events (newapi_request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_service_errors_group_time ON service_error_events (group_key, created_at DESC, id DESC)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func newServiceErrorQueue(db *DB) *serviceErrorQueue {
	ctx, cancel := context.WithCancel(context.Background())
	return &serviceErrorQueue{db: db, jobs: make(chan serviceErrorJob, serviceErrorQueueCapacity), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func serviceErrorString(value string, limit int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.Clone(value)
}

func normalizeServiceError(event ServiceErrorEvent) ServiceErrorEvent {
	// Group metadata is computed for a query window, never persisted as an event.
	event.Group = nil
	for _, field := range []*string{
		&event.ID, &event.RequestID, &event.NewAPIRequestID, &event.Code, &event.ErrorType,
		&event.Stage, &event.Method, &event.Transport, &event.Model, &event.APIKeyName,
		&event.ThreadSource, &event.RequestKind, &event.ThreadID, &event.NewAPIUserID, &event.NewAPIUserName,
	} {
		*field = serviceErrorString(*field, 160)
	}
	if !event.NewAPIIdentityVerified {
		event.NewAPIUserID, event.NewAPIUserName = "", ""
	}
	event.Message = serviceErrorString(event.Message, 2048)
	event.Endpoint = serviceErrorString(event.Endpoint, 256)
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	event.CreatedAt = event.CreatedAt.UTC().Truncate(time.Millisecond)
	if len(event.ToolProtocol) > 4096 || !json.Valid(event.ToolProtocol) {
		event.ToolProtocol = nil
	} else {
		event.ToolProtocol = append(json.RawMessage(nil), event.ToolProtocol...)
	}
	clientInfo := make(map[string]string)
	keys := make([]string, 0, min(len(event.ClientInfo), 24))
	for key := range event.ClientInfo {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	remaining := 4096
	for _, key := range keys {
		if len(clientInfo) == 24 {
			break
		}
		name, value := serviceErrorString(key, 160), serviceErrorString(event.ClientInfo[key], 512)
		if name == "" || value == "" {
			continue
		}
		if len(name)+len(value) > remaining {
			break
		}
		clientInfo[name] = value
		remaining -= len(name) + len(value)
	}
	event.ClientInfo = clientInfo
	return event
}

// EnqueueServiceError never blocks the caller. It returns false when the
// event is invalid, the collector is closed, or the bounded queue is full.
func (db *DB) EnqueueServiceError(event ServiceErrorEvent) bool {
	if db == nil || db.serviceErrors == nil || event.ID == "" || event.StatusCode < 400 || event.StatusCode > 599 {
		return false
	}
	queue := db.serviceErrors
	event = normalizeServiceError(event)
	payload, err := json.Marshal(event)
	if err != nil {
		return false
	}
	queue.mu.RLock()
	defer queue.mu.RUnlock()
	if queue.closed {
		queue.dropped.Add(1)
		return false
	}
	queue.pending.Add(1)
	select {
	case queue.jobs <- serviceErrorJob{event: event, payload: string(payload)}:
		return true
	default:
		queue.pending.Add(-1)
		queue.dropped.Add(1)
		queue.warn("queue_full")
		return false
	}
}

func (queue *serviceErrorQueue) warn(reason string) {
	now := time.Now().Unix()
	previous := queue.lastWarn.Load()
	if now-previous >= 60 && queue.lastWarn.CompareAndSwap(previous, now) {
		log.Printf("service_error_collector reason=%s dropped=%d write_failures=%d", reason, queue.dropped.Load(), queue.failed.Load())
	}
}

func (queue *serviceErrorQueue) run() {
	defer close(queue.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := make([]serviceErrorJob, 0, serviceErrorBatchSize)
	lastCleanup := time.Time{}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(queue.ctx, 2*time.Second)
		err := queue.db.insertServiceErrors(ctx, batch)
		cancel()
		if err != nil {
			queue.failed.Add(uint64(len(batch)))
			queue.warn("write_failed")
		} else {
			queue.written.Add(uint64(len(batch)))
		}
		queue.pending.Add(-int64(len(batch)))
		clear(batch)
		batch = batch[:0]
	}
	for {
		select {
		case job, open := <-queue.jobs:
			if !open {
				flush()
				return
			}
			batch = append(batch, job)
			if len(batch) == serviceErrorBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
			if time.Since(lastCleanup) >= time.Minute {
				ctx, cancel := context.WithTimeout(queue.ctx, 2*time.Second)
				if err := queue.db.pruneServiceErrors(ctx, time.Now(), serviceErrorMaxRows); err != nil {
					queue.warn("retention_failed")
				}
				cancel()
				lastCleanup = time.Now()
			}
		case <-queue.ctx.Done():
			lost := len(batch)
			for range queue.jobs {
				lost++
			}
			queue.dropped.Add(uint64(lost))
			queue.pending.Add(-int64(lost))
			return
		}
	}
}

// close drains the queue for up to three seconds, then cancels the writer.
func (queue *serviceErrorQueue) close() {
	queue.mu.Lock()
	if !queue.closed {
		queue.closed = true
		close(queue.jobs)
	}
	queue.mu.Unlock()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-queue.done:
	case <-timer.C:
		queue.cancel()
		<-queue.done
	}
	queue.cancel()
}

func (db *DB) insertServiceErrors(ctx context.Context, jobs []serviceErrorJob) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statement, err := tx.PrepareContext(ctx, `INSERT INTO service_error_events
			(id, created_at, status_code, stage, request_id, newapi_request_id, payload, group_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (id) DO NOTHING`)
		if err != nil {
			return err
		}
		defer statement.Close()
		for _, job := range jobs {
			event := job.event
			if _, err := statement.ExecContext(ctx, event.ID, event.CreatedAt.UnixMilli(), event.StatusCode, event.Stage, event.RequestID, event.NewAPIRequestID, job.payload, serviceErrorGroupKey(event)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) pruneServiceErrors(ctx context.Context, now time.Time, maxRows int) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM service_error_events WHERE created_at < $1`, now.Add(-serviceErrorRetention).UnixMilli()); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `DELETE FROM service_error_events WHERE id IN (
			SELECT id FROM service_error_events ORDER BY created_at DESC, id DESC LIMIT 2147483647 OFFSET $1
		)`, maxRows)
		return err
	})
}

func (db *DB) ServiceErrorCollectorStats() ServiceErrorCollectorStats {
	stats := ServiceErrorCollectorStats{Capacity: serviceErrorQueueCapacity, RetentionDays: 7, MaxRows: serviceErrorMaxRows}
	if db != nil && db.serviceErrors != nil {
		queue := db.serviceErrors
		stats.Pending, stats.Written = queue.pending.Load(), queue.written.Load()
		stats.Dropped, stats.WriteFailures = queue.dropped.Load(), queue.failed.Load()
	}
	return stats
}

type serviceErrorCursor struct {
	CreatedAt int64  `json:"time"`
	ID        string `json:"id"`
	Grouped   bool   `json:"grouped,omitempty"`
}

func decodeServiceErrorCursor(value string) (serviceErrorCursor, error) {
	var cursor serviceErrorCursor
	if value == "" {
		return cursor, nil
	}
	if len(value) > 512 {
		return cursor, fmt.Errorf("invalid cursor")
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(payload, &cursor) != nil || cursor.CreatedAt <= 0 || cursor.ID == "" || len(cursor.ID) > 160 {
		return cursor, fmt.Errorf("invalid cursor")
	}
	return cursor, nil
}

func encodeServiceErrorCursor(cursor serviceErrorCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

// ValidateServiceErrorViewCursor reports whether a cursor is well formed and
// belongs to the requested view; grouped and individual cursors do not mix.
func ValidateServiceErrorViewCursor(value string, grouped bool) bool {
	cursor, err := decodeServiceErrorCursor(value)
	return err == nil && (value == "" || cursor.Grouped == grouped)
}

func (db *DB) ListServiceErrors(ctx context.Context, filter ServiceErrorFilter) (ServiceErrorPage, error) {
	page := ServiceErrorPage{Items: []ServiceErrorEvent{}, Collector: db.ServiceErrorCollectorStats(), Grouped: filter.Grouped}
	cursor, err := decodeServiceErrorCursor(filter.Cursor)
	if err != nil {
		return page, err
	}
	if filter.Cursor != "" && cursor.Grouped != filter.Grouped {
		return page, fmt.Errorf("cursor belongs to another view")
	}
	if !ValidateServiceErrorGroupKey(filter.GroupKey) || filter.Grouped && filter.GroupKey != "" {
		return page, fmt.Errorf("invalid group filter")
	}
	if filter.Start.IsZero() || !filter.End.After(filter.Start) || filter.End.Sub(filter.Start) > serviceErrorRetention {
		return page, fmt.Errorf("invalid time range")
	}
	conditions := []string{"created_at >= $1", "created_at <= $2"}
	args := []any{filter.Start.UnixMilli(), filter.End.UnixMilli()}
	add := func(condition string, value any) {
		args = append(args, value)
		conditions = append(conditions, strings.ReplaceAll(condition, "?", fmt.Sprintf("$%d", len(args))))
	}
	switch filter.Status {
	case "4xx":
		conditions = append(conditions, "status_code >= 400 AND status_code < 500")
	case "5xx":
		conditions = append(conditions, "status_code >= 500")
	case "429":
		add("status_code = ?", 429)
	case "":
	default:
		return page, fmt.Errorf("invalid status")
	}
	if filter.Stage != "" {
		add("stage = ?", filter.Stage)
	}
	if filter.RequestID != "" {
		add("(request_id = ? OR newapi_request_id = ?)", filter.RequestID)
	}
	if filter.GroupKey != "" {
		add("group_key = ?", filter.GroupKey)
	}
	where := strings.Join(conditions, " AND ")
	err = db.conn.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 500 THEN 1 ELSE 0 END), 0),
		COUNT(DISTINCT COALESCE(NULLIF(group_key, ''), id))
		FROM service_error_events WHERE `+where, args...).Scan(&page.Summary.Total, &page.Summary.Status429, &page.Summary.Status4xx, &page.Summary.Status5xx, &page.Summary.Groups)
	if err != nil {
		return page, err
	}
	if filter.Grouped {
		return db.listServiceErrorGroups(ctx, filter, cursor, page, where, args)
	}
	if filter.Cursor != "" {
		args = append(args, cursor.CreatedAt, cursor.ID)
		where += fmt.Sprintf(" AND (created_at < $%d OR (created_at = $%d AND id < $%d))", len(args)-1, len(args)-1, len(args))
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	args = append(args, limit+1)
	rows, err := db.conn.QueryContext(ctx, `SELECT payload FROM service_error_events WHERE `+where+fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		var event ServiceErrorEvent
		if err := rows.Scan(&payload); err != nil {
			return page, err
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return page, err
		}
		page.Items = append(page.Items, event)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeServiceErrorCursor(serviceErrorCursor{CreatedAt: last.CreatedAt.UnixMilli(), ID: last.ID})
	}
	return page, rows.Err()
}
