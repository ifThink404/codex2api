package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func insertGroupedTestEvents(t *testing.T, db *DB, events []ServiceErrorEvent) {
	t.Helper()
	jobs := make([]serviceErrorJob, 0, len(events))
	for _, event := range events {
		event = normalizeServiceError(event)
		payload, err := json.Marshal(event)
		require.NoError(t, err)
		jobs = append(jobs, serviceErrorJob{event: event, payload: string(payload)})
	}
	require.NoError(t, db.insertServiceErrors(t.Context(), jobs))
}

func TestServiceErrorGroupingAcrossPagesAndDetails(t *testing.T) {
	db := newProxyTestDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := ServiceErrorEvent{CreatedAt: now, APIKeyID: 1, NewAPIIdentityVerified: true, NewAPIUserID: "3687", NewAPIUserName: "example",
		StatusCode: 503, Code: "service_unavailable", Stage: "dispatch", Method: "POST", Endpoint: "/v1/alpha/search", Model: "gpt-6-astra", Message: "Service temporarily unavailable"}
	var events []ServiceErrorEvent
	for i := 0; i < 53; i++ {
		event := base
		event.ID, event.RequestID, event.NewAPIRequestID = fmt.Sprintf("event-%03d", i), fmt.Sprintf("req-%d", i), fmt.Sprintf("newapi-%d", i)
		event.CreatedAt = now.Add(time.Duration(i-53) * time.Second)
		events = append(events, event)
	}
	// Same timestamp: the representative is picked by ID, and pagination must
	// not discard other groups that have the same latest timestamp.
	other := base
	other.ID, other.NewAPIUserID = "another-user", "other"
	events = append(events, other)
	last := base
	last.ID, last.StatusCode = "z-another-status", 429
	events = append(events, last)
	insertGroupedTestEvents(t, db, events)
	filter := ServiceErrorFilter{Start: now.Add(-time.Hour), End: now, Grouped: true, Limit: 1}
	page, err := db.ListServiceErrors(t.Context(), filter)
	require.NoError(t, err)
	require.EqualValues(t, 55, page.Summary.Total)
	require.EqualValues(t, 3, page.Summary.Groups)
	require.EqualValues(t, 1, page.Summary.Status429)
	require.True(t, page.Grouped)
	require.Equal(t, last.ID, page.Items[0].ID)
	require.NotEmpty(t, page.NextCursor)
	filter.Cursor = page.NextCursor
	page, err = db.ListServiceErrors(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, other.ID, page.Items[0].ID)
	filter.Cursor = page.NextCursor
	page, err = db.ListServiceErrors(t.Context(), filter)
	require.NoError(t, err)
	require.Empty(t, page.NextCursor)
	require.Len(t, page.Items, 1)
	group := page.Items[0].Group
	require.EqualValues(t, 53, group.Count, "group counts must cover more than one raw page")
	require.Equal(t, "event-052", page.Items[0].ID)
	require.Equal(t, events[0].CreatedAt, group.FirstSeen)
	require.Equal(t, events[52].CreatedAt, group.LastSeen)

	filter.Cursor, filter.Grouped, filter.GroupKey, filter.Limit = "", false, group.Key, 20
	seen := map[string]bool{}
	for {
		page, err = db.ListServiceErrors(t.Context(), filter)
		require.NoError(t, err)
		require.EqualValues(t, 53, page.Summary.Total)
		for _, event := range page.Items {
			require.False(t, seen[event.ID], "raw record duplicated between pages")
			seen[event.ID] = true
			require.Nil(t, event.Group)
			require.NotEmpty(t, event.NewAPIRequestID)
		}
		if page.NextCursor == "" {
			break
		}
		filter.Cursor = page.NextCursor
	}
	require.Len(t, seen, 53)
	filter.Cursor, filter.RequestID = "", "newapi-20"
	page, err = db.ListServiceErrors(t.Context(), filter)
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Summary.Total)
	require.Equal(t, "event-020", page.Items[0].ID)
	filter.RequestID, filter.Stage = "", "rate_limit"
	page, err = db.ListServiceErrors(t.Context(), filter)
	require.NoError(t, err)
	require.Zero(t, page.Summary.Total)
	filter.Stage, filter.GroupKey, filter.Grouped = "", "", true
	filter.Start = now.Add(-10 * time.Second)
	page, err = db.ListServiceErrors(t.Context(), filter)
	require.NoError(t, err)
	require.EqualValues(t, 12, page.Summary.Total)
	require.EqualValues(t, 3, page.Summary.Groups)
	for _, event := range page.Items {
		if event.ID == "event-052" {
			require.EqualValues(t, 10, event.Group.Count, "time filters apply before aggregation")
			require.Equal(t, filter.Start, event.Group.FirstSeen)
		}
	}
}

func TestServiceErrorGroupingIdentityAndCauses(t *testing.T) {
	base := ServiceErrorEvent{APIKeyID: 1, NewAPIIdentityVerified: true, NewAPIUserID: "7", ScopeHash: "window-a", StatusCode: 503,
		Code: "service_unavailable", Stage: "dispatch", Endpoint: "/v1/responses", Method: "POST", Model: "m", RequestID: "req-one", Message: "failed: req-one",
		CandidateRejections: []string{"account_paused", "request_excluded"}}
	want := serviceErrorGroupKey(base)
	repeated := base
	repeated.RequestID, repeated.Message = "req-two", "failed: req-two"
	repeated.ScopeHash, repeated.ThreadID, repeated.NewAPIUserName, repeated.DurationMs = "window-b", "another-thread", "renamed", 123
	repeated.CandidateRejections = []string{"request_excluded", "account_paused", "account_paused"}
	require.Equal(t, want, serviceErrorGroupKey(repeated))
	for name, change := range map[string]func(*ServiceErrorEvent){
		"user":       func(e *ServiceErrorEvent) { e.NewAPIUserID = "8" },
		"key":        func(e *ServiceErrorEvent) { e.APIKeyID = 2 },
		"unverified": func(e *ServiceErrorEvent) { e.NewAPIIdentityVerified = false },
		"endpoint":   func(e *ServiceErrorEvent) { e.Endpoint = "/v1/alpha/search" },
		"model":      func(e *ServiceErrorEvent) { e.Model = "other" },
		"status":     func(e *ServiceErrorEvent) { e.StatusCode = 429 },
		"code":       func(e *ServiceErrorEvent) { e.Code = "other" },
		"message":    func(e *ServiceErrorEvent) { e.Message = "different cause" },
		"cause":      func(e *ServiceErrorEvent) { e.CandidateRejections = []string{"model_or_provider_mismatch"} },
	} {
		t.Run(name, func(t *testing.T) {
			event := base
			change(&event)
			require.NotEqual(t, want, serviceErrorGroupKey(event))
		})
	}
	base.NewAPIIdentityVerified = false
	repeated = base
	repeated.ScopeHash = "another-caller"
	require.NotEqual(t, serviceErrorGroupKey(base), serviceErrorGroupKey(repeated))
}

func TestServiceErrorGroupingUpgradeKeepsOriginalRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = conn.Exec(`CREATE TABLE service_error_events (id TEXT PRIMARY KEY, created_at BIGINT NOT NULL, status_code INTEGER NOT NULL, stage TEXT NOT NULL, request_id TEXT NOT NULL, newapi_request_id TEXT NOT NULL, payload TEXT NOT NULL)`)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 130; i++ {
		event := ServiceErrorEvent{ID: fmt.Sprintf("legacy-%03d", i), CreatedAt: now, StatusCode: 503, Stage: "dispatch", Message: "unavailable"}
		payload, err := json.Marshal(event)
		require.NoError(t, err)
		_, err = conn.Exec(`INSERT INTO service_error_events VALUES (?, ?, 503, 'dispatch', '', '', ?)`, event.ID, now.UnixMilli(), string(payload))
		require.NoError(t, err)
	}
	require.NoError(t, conn.Close())
	db, err := New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	filter := ServiceErrorFilter{Start: now.Add(-time.Minute), End: now.Add(time.Minute), Grouped: true}
	require.Eventually(t, func() bool {
		page, err := db.ListServiceErrors(t.Context(), filter)
		return err == nil && page.Summary.GroupingPending == 0 && len(page.Items) == 1 && page.Items[0].Group.Count == 130
	}, 5*time.Second, 20*time.Millisecond)
	var count int
	require.NoError(t, db.conn.QueryRow(`SELECT COUNT(*) FROM service_error_events`).Scan(&count))
	require.Equal(t, 130, count)
	require.NoError(t, db.backfillServiceErrorGroups(context.Background()), "backfill is idempotent")
}
