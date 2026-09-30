package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Group by stable audit evidence, never just by rule names or score. Current
// request previews may change while the same developer/tool evidence repeats.
// Keep request IDs, timestamps, latency and session IDs in individual records.
func promptLogGroupExpressions(prefix string) []string {
	var fields []string
	auxiliaryEvidence := prefix + "primary_origin IN ('developer', 'system', 'instructions', 'history', 'tool_output', 'tool_arguments', 'attachment_refs', 'attachment_content', 'session_context', 'application_candidate') AND COALESCE(" + prefix + "match_context, '') <> ''"
	clearedAuxiliaryReview := "(" + auxiliaryEvidence + ") AND COALESCE(" + prefix + "reviewed, false) = true AND COALESCE(" + prefix + "review_flagged, false) = false AND COALESCE(TRIM(" + prefix + "review_error), '') = ''"
	for _, column := range []string{"source", "request_protocol", "request_provider", "endpoint", "model", "action", "mode", "policy_profile", "reason_code", "primary_origin", "newapi_policy_status", "newapi_platform", "newapi_user_id", "matched_patterns", "error_code", "review_model", "review_error", "review_endpoint", "review_request_mode"} {
		fields = append(fields, "COALESCE("+prefix+column+", '')")
	}
	for _, column := range []string{"api_key_id", "score", "audit_score", "threshold_value"} {
		fields = append(fields, "COALESCE("+prefix+column+", 0)")
	}
	for _, column := range []string{"reviewed", "review_flagged", "strike_eligible"} {
		fields = append(fields, "COALESCE("+prefix+column+", false)")
	}
	for _, column := range []string{"review_confidence", "review_threshold"} {
		// A passing review may inspect a different current-user prompt on every
		// request. Its probability/category threshold is not auxiliary evidence.
		fields = append(fields, "CASE WHEN "+clearedAuxiliaryReview+" THEN -1 ELSE COALESCE("+prefix+column+", -1) END")
	}
	fields = append(fields, "CASE WHEN "+clearedAuxiliaryReview+" THEN '' ELSE COALESCE("+prefix+"review_reason, '') END")
	fields = append(fields,
		"CASE WHEN COALESCE("+prefix+"newapi_user_id, '') <> '' AND "+prefix+"newapi_policy_status IN ('verified', 'signed_response') THEN '' ELSE COALESCE("+prefix+"client_ip, '') END",
		// Auxiliary audit groups follow their own evidence, not the unrelated
		// current-user preview. User-origin records must retain prompt identity.
		"CASE WHEN "+auxiliaryEvidence+" THEN "+prefix+"match_context ELSE COALESCE(NULLIF("+prefix+"full_text, ''), NULLIF("+prefix+"text_preview, ''), "+prefix+"match_context, '') END",
	)
	return fields
}

func (db *DB) listPromptFilterLogGroups(ctx context.Context, query PromptFilterLogQuery) ([]*PromptFilterLog, int, error) {
	size := query.PageSize
	if size <= 0 {
		size = query.Limit
	}
	if size <= 0 || size > 500 {
		size = 100
	}
	page := query.Page
	if page <= 0 {
		page = 1
	}
	where, args := promptFilterLogWhere(query)
	partition := strings.Join(promptLogGroupExpressions(""), ", ")
	var total int
	countArgs := append([]any(nil), args...)
	args = append(args, size, (page-1)*size)
	rows, err := db.conn.QueryContext(ctx, `WITH audit_groups AS (
		SELECT MAX(id) AS id, COUNT(*) AS occurrence_count, MIN(created_at) AS first_seen,
		MAX(created_at) AS last_seen, MAX(created_at) AS created_at, MAX(audit_score) AS audit_score
		FROM prompt_filter_logs`+where+` GROUP BY `+partition+`)
		SELECT id, occurrence_count, first_seen, last_seen, COUNT(*) OVER () FROM audit_groups
		ORDER BY `+promptFilterLogOrder(query.Sort)+fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	type group struct {
		id, count   int64
		first, last time.Time
	}
	var groups []group
	for rows.Next() {
		var item group
		var first, last any
		if err = rows.Scan(&item.id, &item.count, &first, &last, &total); err != nil {
			break
		}
		if item.first, err = parsePromptLogGroupTime(first); err != nil {
			break
		}
		if item.last, err = parsePromptLogGroupTime(last); err != nil {
			break
		}
		groups = append(groups, item)
	}
	rowErr := rows.Err()
	rows.Close() // Release SQLite's connection before loading representatives.
	if err != nil {
		return nil, 0, err
	}
	if rowErr != nil {
		return nil, 0, rowErr
	}
	if len(groups) == 0 {
		// An out-of-range page has no row carrying the window count. Only this
		// uncommon path needs a separate aggregate count query.
		if page > 1 {
			if err := db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT 1 FROM prompt_filter_logs"+where+" GROUP BY "+partition+") audit_groups", countArgs...).Scan(&total); err != nil {
				return nil, 0, err
			}
		}
		return []*PromptFilterLog{}, total, nil
	}
	query.Grouped = false
	query.Page = 1
	query.PageSize = len(groups)
	for _, group := range groups {
		query.ids = append(query.ids, group.id)
	}
	representatives, _, err := db.ListPromptFilterLogsPage(ctx, query)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[int64]*PromptFilterLog, len(representatives))
	for _, item := range representatives {
		byID[item.ID] = item
	}
	logs := make([]*PromptFilterLog, 0, len(groups))
	for _, group := range groups {
		item := byID[group.id]
		if item == nil {
			continue
		} // A concurrent retention sweep may remove it.
		if !item.CreatedAt.Equal(group.last) {
			latestQuery := query
			latestQuery.ids = nil
			latestQuery.GroupID = group.id
			latestQuery.Sort = "newest"
			latestQuery.PageSize = 1
			latest, _, err := db.ListPromptFilterLogsPage(ctx, latestQuery)
			if err != nil {
				return nil, 0, err
			}
			if len(latest) == 0 {
				continue
			}
			item = latest[0]
		}
		item.GroupID = group.id
		item.OccurrenceCount = group.count
		item.FirstSeen = &group.first
		item.LastSeen = &group.last
		logs = append(logs, item)
	}
	return logs, total, nil
}

// SQLite MIN/MAX over created_at can return the driver's time.String() form
// ("2006-01-02 15:04:05.999999999 -0700 MST") instead of the stored layout.
func parsePromptLogGroupTime(value any) (time.Time, error) {
	parsed, err := parseDBTimeValue(value)
	if err == nil {
		return parsed, nil
	}
	if text, ok := value.(string); ok {
		if fallback, fallbackErr := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", text); fallbackErr == nil {
			return fallback, nil
		}
	}
	return time.Time{}, err
}
