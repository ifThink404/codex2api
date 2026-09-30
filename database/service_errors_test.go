package database

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func newServiceErrorTestDB(test *testing.T) *DB {
	test.Helper()
	db, err := New("sqlite", filepath.Join(test.TempDir(), "service-errors.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	return db
}

func waitServiceErrorQueue(test *testing.T, db *DB) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for db.ServiceErrorCollectorStats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stats := db.ServiceErrorCollectorStats(); stats.Pending != 0 {
		test.Fatalf("collector did not drain: %+v", stats)
	}
}

func insertServiceErrorTestEvents(test *testing.T, db *DB, events []ServiceErrorEvent) {
	test.Helper()
	jobs := make([]serviceErrorJob, 0, len(events))
	for _, event := range events {
		event = normalizeServiceError(event)
		payload, err := json.Marshal(event)
		if err != nil {
			test.Fatal(err)
		}
		jobs = append(jobs, serviceErrorJob{event: event, payload: string(payload)})
	}
	if err := db.insertServiceErrors(context.Background(), jobs); err != nil {
		test.Fatal(err)
	}
}

func TestServiceErrorsPersistencePaginationAndIsolation(test *testing.T) {
	path := filepath.Join(test.TempDir(), "service-errors.db")
	db, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for index := 0; index < 4; index++ {
		event := ServiceErrorEvent{ID: fmt.Sprintf("event-%d", index), CreatedAt: now, StatusCode: 429, Stage: "rate_limit", RequestID: "request-one", NewAPIRequestID: "newapi-one", Message: "concurrency exhausted"}
		event.NewAPIIdentityVerified, event.NewAPIUserID, event.NewAPIUserName = true, "1881", "示例用户"
		if index == 3 {
			event.StatusCode, event.Stage = 400, "validation"
		}
		if !db.EnqueueServiceError(event) {
			test.Fatal("event rejected")
		}
	}
	// Close drains the queue; the rows must survive a restart.
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	filter := ServiceErrorFilter{Start: now.Add(-time.Hour), End: now.Add(time.Second), Limit: 2}
	first, err := db.ListServiceErrors(context.Background(), filter)
	if err != nil || len(first.Items) != 2 || first.Summary.Total != 4 || first.Summary.Status429 != 3 || first.NextCursor == "" || first.Items[0].ID != "event-3" {
		test.Fatalf("first page=%+v err=%v", first, err)
	}
	if !first.Items[0].NewAPIIdentityVerified || first.Items[0].NewAPIUserID != "1881" || first.Items[0].NewAPIUserName != "示例用户" {
		test.Fatalf("NewAPI user not persisted: %+v", first.Items[0])
	}
	filter.Cursor = first.NextCursor
	second, err := db.ListServiceErrors(context.Background(), filter)
	if err != nil || len(second.Items) != 2 || second.NextCursor != "" || second.Items[0].ID != "event-1" || second.Summary.Total != 4 {
		test.Fatalf("second page=%+v err=%v", second, err)
	}
	filter.Cursor, filter.Status, filter.RequestID = "", "429", "newapi-one"
	filtered, err := db.ListServiceErrors(context.Background(), filter)
	if err != nil || filtered.Summary.Total != 3 {
		test.Fatalf("NewAPI request filter=%+v err=%v", filtered, err)
	}
	filter.RequestID, filter.Stage = "request-one", "validation"
	filtered, err = db.ListServiceErrors(context.Background(), filter)
	if err != nil || filtered.Summary.Total != 0 {
		test.Fatalf("combined filter=%+v err=%v", filtered, err)
	}
	filter.RequestID, filter.Stage, filter.Status = "' OR 1=1 --", "", ""
	filtered, err = db.ListServiceErrors(context.Background(), filter)
	if err != nil || filtered.Summary.Total != 0 {
		test.Fatalf("unsafe request filter=%+v err=%v", filtered, err)
	}
	for _, table := range []string{"usage_logs", "accounts"} {
		var count int
		if err := db.conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			test.Fatalf("service errors changed %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestServiceErrorNewAPIUserNormalization(test *testing.T) {
	event := normalizeServiceError(ServiceErrorEvent{NewAPIIdentityVerified: true, NewAPIUserID: " 1881 ", NewAPIUserName: strings.Repeat("中文", 100)})
	if event.NewAPIUserID != "1881" || len(event.NewAPIUserName) > 160 || !utf8.ValidString(event.NewAPIUserName) || event.NewAPIUserName == "" {
		test.Fatalf("invalid normalized identity: %+v", event)
	}
	event.NewAPIIdentityVerified = false
	event = normalizeServiceError(event)
	payload, err := json.Marshal(event)
	if err != nil || strings.Contains(string(payload), "newapi_user_") {
		test.Fatalf("unverified user retained: %s err=%v", payload, err)
	}
}

func TestServiceErrorQueueBoundedAndImmutable(test *testing.T) {
	db := &DB{}
	db.serviceErrors = newServiceErrorQueue(db)
	defer db.serviceErrors.cancel()
	clientInfo := map[string]string{"user_agent": "codex_cli_rs/0.150.0"}
	event := ServiceErrorEvent{ID: "bounded", StatusCode: 429, Message: strings.Repeat("中文", 3000), ClientInfo: clientInfo}
	for index := 0; index < serviceErrorQueueCapacity; index++ {
		if !db.EnqueueServiceError(event) {
			test.Fatalf("queue full at %d", index)
		}
	}
	if db.EnqueueServiceError(event) {
		test.Fatal("queue exceeded capacity")
	}
	clientInfo["user_agent"] = "mutated"
	job := <-db.serviceErrors.jobs
	if len(job.event.Message) > 2048 || !utf8.ValidString(job.event.Message) || job.event.ClientInfo["user_agent"] != "codex_cli_rs/0.150.0" || !json.Valid([]byte(job.payload)) {
		test.Fatalf("unbounded or mutable event: %+v", job.event)
	}
	if stats := db.ServiceErrorCollectorStats(); stats.Pending != serviceErrorQueueCapacity || stats.Dropped != 1 {
		test.Fatalf("unexpected collector counters: %+v", stats)
	}
}

func TestServiceErrorRetentionAndRowLimit(test *testing.T) {
	db := newServiceErrorTestDB(test)
	now := time.Now().UTC()
	for index := 0; index < 5; index++ {
		created := now.Add(time.Duration(index-5) * time.Minute)
		if index == 0 {
			created = now.Add(-8 * 24 * time.Hour)
		}
		db.EnqueueServiceError(ServiceErrorEvent{ID: fmt.Sprint(index), CreatedAt: created, StatusCode: 500})
	}
	waitServiceErrorQueue(test, db)
	if err := db.pruneServiceErrors(context.Background(), now, 2); err != nil {
		test.Fatal(err)
	}
	var count int
	if err := db.conn.QueryRow("SELECT COUNT(*) FROM service_error_events").Scan(&count); err != nil || count != 2 {
		test.Fatalf("retention count=%d err=%v", count, err)
	}
	page, err := db.ListServiceErrors(context.Background(), ServiceErrorFilter{Start: now.Add(-time.Hour), End: now, Limit: 20})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "4" || page.Items[1].ID != "3" {
		test.Fatalf("retention page=%+v err=%v", page, err)
	}
}

func TestServiceErrorWriteFailuresAreVisible(test *testing.T) {
	db := newServiceErrorTestDB(test)
	if _, err := db.conn.Exec("DROP TABLE service_error_events"); err != nil {
		test.Fatal(err)
	}
	if !db.EnqueueServiceError(ServiceErrorEvent{ID: "failed", StatusCode: 429}) {
		test.Fatal("enqueue failed")
	}
	waitServiceErrorQueue(test, db)
	if stats := db.ServiceErrorCollectorStats(); stats.WriteFailures != 1 || stats.Written != 0 {
		test.Fatalf("failure not visible: %+v", stats)
	}
}

func TestServiceErrorCursorValidation(test *testing.T) {
	for _, value := range []string{"not-a-cursor", strings.Repeat("x", 513), "e30", "bnVsbA"} {
		if ValidateServiceErrorViewCursor(value, false) {
			test.Errorf("accepted cursor %q", value)
		}
	}
	grouped := encodeServiceErrorCursor(serviceErrorCursor{CreatedAt: 1, ID: "a", Grouped: true})
	if ValidateServiceErrorViewCursor(grouped, false) || !ValidateServiceErrorViewCursor(grouped, true) {
		test.Fatal("grouped and individual cursors must not mix")
	}
}

func TestServiceErrorGroupingAcrossPagesAndDetails(test *testing.T) {
	db := newServiceErrorTestDB(test)
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := ServiceErrorEvent{CreatedAt: now, APIKeyID: 1, NewAPIIdentityVerified: true, NewAPIUserID: "3687", NewAPIUserName: "example",
		StatusCode: 503, Code: "service_unavailable", Stage: "dispatch", Method: "POST", Endpoint: "/v1/alpha/search", Model: "gpt-6-astra", Message: "Service temporarily unavailable"}
	var events []ServiceErrorEvent
	for index := 0; index < 53; index++ {
		event := base
		event.ID, event.RequestID, event.NewAPIRequestID = fmt.Sprintf("event-%03d", index), fmt.Sprintf("req-%d", index), fmt.Sprintf("newapi-%d", index)
		event.Message = "Service temporarily unavailable " + event.RequestID
		event.CreatedAt = now.Add(time.Duration(index-53) * time.Second)
		events = append(events, event)
	}
	// Same timestamp: the representative is picked by ID, and pagination must
	// not discard other groups that share the latest timestamp.
	other := base
	other.ID, other.NewAPIUserID = "another-user", "other"
	last := base
	last.ID, last.StatusCode = "z-another-status", 429
	events = append(events, other, last)
	insertServiceErrorTestEvents(test, db, events)

	filter := ServiceErrorFilter{Start: now.Add(-time.Hour), End: now, Grouped: true, Limit: 1}
	page, err := db.ListServiceErrors(context.Background(), filter)
	if err != nil || page.Summary.Total != 55 || page.Summary.Groups != 3 || page.Summary.Status429 != 1 || !page.Grouped || page.Items[0].ID != last.ID || page.NextCursor == "" {
		test.Fatalf("first grouped page=%+v err=%v", page, err)
	}
	filter.Cursor = page.NextCursor
	if page, err = db.ListServiceErrors(context.Background(), filter); err != nil || page.Items[0].ID != other.ID {
		test.Fatalf("second grouped page=%+v err=%v", page, err)
	}
	filter.Cursor = page.NextCursor
	page, err = db.ListServiceErrors(context.Background(), filter)
	if err != nil || page.NextCursor != "" || len(page.Items) != 1 || page.Items[0].Group == nil {
		test.Fatalf("last grouped page=%+v err=%v", page, err)
	}
	group := page.Items[0].Group
	if group.Count != 53 || page.Items[0].ID != "event-052" || !group.FirstSeen.Equal(events[0].CreatedAt) || !group.LastSeen.Equal(events[52].CreatedAt) {
		test.Fatalf("group counts must cover more than one raw page: %+v %+v", page.Items[0], group)
	}

	filter.Cursor, filter.Grouped, filter.GroupKey, filter.Limit = "", false, group.Key, 20
	seen := map[string]bool{}
	for {
		page, err = db.ListServiceErrors(context.Background(), filter)
		if err != nil || page.Summary.Total != 53 {
			test.Fatalf("group details page=%+v err=%v", page, err)
		}
		for _, event := range page.Items {
			if seen[event.ID] {
				test.Fatalf("raw record %s duplicated between pages", event.ID)
			}
			seen[event.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		filter.Cursor = page.NextCursor
	}
	if len(seen) != 53 {
		test.Fatalf("group details returned %d records, want 53", len(seen))
	}
	if _, err := db.ListServiceErrors(context.Background(), ServiceErrorFilter{Start: now.Add(-time.Hour), End: now, Grouped: true, GroupKey: group.Key}); err == nil {
		test.Fatal("grouped view must reject a group_key filter")
	}
}
