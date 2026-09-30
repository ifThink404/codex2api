package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWalkUsageLogsByFilterStreamsAllMatchesWithoutPageLimit(t *testing.T) {
	db := newGrokStateTestDB(t)
	now := time.Now().UTC()
	for index := 0; index < 503; index++ {
		if err := db.InsertUsageLog(t.Context(), &UsageLogInput{
			Endpoint: "/v1/responses", Model: "export-model", RequestID: fmt.Sprintf("export-%d", index),
			Channel: "codex", StatusCode: 500, ClientUserAgent: "Codex Desktop/0.150.0",
		}); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.FlushUsageLogs()
	// The background flusher may hold the final batch while FlushUsageLogs
	// sees an empty buffer; wait until every inserted row is committed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var stored int
		if err := db.conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_logs WHERE model = 'export-model'`).Scan(&stored); err != nil {
			t.Fatalf("count usage logs: %v", err)
		}
		if stored == 503 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored usage logs = %d, want 503", stored)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := db.conn.ExecContext(t.Context(), `INSERT INTO usage_logs (status_code, channel, created_at)
		VALUES (200, 'grok', $1), (499, 'grok', $2)`, sqliteTimeParam(now.Add(-365*24*time.Hour)), sqliteTimeParam(now)); err != nil {
		t.Fatalf("insert extra rows: %v", err)
	}
	filter := UsageLogFilter{Start: now.Add(-time.Hour), End: now.Add(time.Hour), Page: 2, PageSize: 1,
		Query: "codex desktop/0.150", Channel: "codex", StatusCode: 500, Model: "export-model"}
	seen := make(map[int64]bool)
	var previous time.Time
	if err := db.WalkUsageLogsByFilter(t.Context(), filter, func(entry *UsageLog) error {
		if seen[entry.ID] {
			t.Fatalf("row %d visited twice", entry.ID)
		}
		if !previous.IsZero() && entry.CreatedAt.After(previous) {
			t.Fatal("rows must be newest first")
		}
		previous = entry.CreatedAt
		seen[entry.ID] = true
		return nil
	}); err != nil {
		t.Fatalf("walk filtered: %v", err)
	}
	if len(seen) != 503 {
		t.Fatalf("filtered rows = %d, want 503 (page/page_size must not apply)", len(seen))
	}
	allCount := 0
	all := UsageLogFilter{Start: time.Unix(0, 0).UTC(), End: now.Add(time.Hour), IncludeCanceled: true}
	if err := db.WalkUsageLogsByFilter(t.Context(), all, func(*UsageLog) error { allCount++; return nil }); err != nil {
		t.Fatalf("walk all: %v", err)
	}
	if allCount != 505 {
		t.Fatalf("all rows = %d, want 505", allCount)
	}
	listed, err := db.ListUsageLogsByFilter(t.Context(), filter)
	if err != nil || len(listed) != 503 {
		t.Fatalf("ListUsageLogsByFilter = %d rows, err %v", len(listed), err)
	}
}

func TestWalkUsageLogsByFilterPropagatesCancellationAndConsumerFailure(t *testing.T) {
	db := newGrokStateTestDB(t)
	if err := db.InsertUsageLog(t.Context(), &UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses"}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: time.Unix(0, 0).UTC(), End: time.Now().Add(time.Hour)}
	consumerError := errors.New("disk full")
	if err := db.WalkUsageLogsByFilter(t.Context(), filter, func(*UsageLog) error { return consumerError }); !errors.Is(err, consumerError) {
		t.Fatalf("consumer error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.WalkUsageLogsByFilter(ctx, filter, func(*UsageLog) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled walk error = %v", err)
	}
	empty, err := db.ListUsageLogsByFilter(t.Context(), UsageLogFilter{Start: time.Now().Add(time.Hour), End: time.Now().Add(2 * time.Hour)})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v err %v, want non-nil empty slice", empty, err)
	}
}
