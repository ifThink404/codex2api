package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ServiceErrorGroup describes repeated occurrences of one error. The
// representative event is the latest original request in the query window;
// counts and time bounds cover every matching event, before pagination.
type ServiceErrorGroup struct {
	Key       string    `json:"key"`
	Count     int64     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

func serviceErrorGroupKey(event ServiceErrorEvent) string {
	caller := "thread:" + event.ThreadID
	if event.NewAPIIdentityVerified && event.NewAPIUserID != "" {
		caller = "newapi:" + event.NewAPIUserID
	}
	message := event.Message
	for _, id := range []string{event.NewAPIRequestID, event.RequestID} {
		if id != "" {
			message = strings.ReplaceAll(message, id, "[request-id]")
		}
	}
	// Names, request IDs and durations do not split repeat failures. Verified
	// users and different causes must remain separate.
	parts := []any{"service-error-v1", event.APIKeyID, caller, event.NewAPIIdentityVerified,
		event.Method, event.Endpoint, event.Transport, event.Model, event.StatusCode,
		event.Stage, event.Code, event.ErrorType, message, event.ThreadSource, event.RequestKind}
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
		page.NextCursor = encodeServiceErrorCursor(serviceErrorCursor{CreatedAt: last.CreatedAt.UnixMilli(), ID: last.ID, Grouped: true})
	}
	return page, rows.Err()
}
