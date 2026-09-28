package database

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPromptAuditGroupsPreserveEvidenceAndFilters(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	base := PromptFilterLogInput{Source: "local_filter", Action: "allow", Mode: "block", Score: 0, AuditScore: 80, Threshold: 50, PrimaryOrigin: "developer", Model: "gpt-test", Endpoint: "/v1/responses", APIKeyID: 1, NewAPIPolicyStatus: "verified", NewAPIPlatform: "test", NewAPIUserID: "1", NewAPIUserName: "Alice", MatchedPatterns: `[{"name":"rule","weight":80}]`, MatchContext: "same complete evidence", TextPreview: "needle first user prompt"}
	insert := func(input PromptFilterLogInput) int64 {
		t.Helper()
		if err := db.InsertPromptFilterLog(ctx, &input); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := db.conn.QueryRowContext(ctx, "SELECT MAX(id) FROM prompt_filter_logs").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := insert(base)
	secondInput := base
	secondInput.TextPreview = "second user prompt"
	secondInput.NewAPIRequestID = "different-request"
	secondInput.SessionHash = "different-session"
	second := insert(secondInput)
	// Ingest order is not necessarily event-time order. Show the newest event.
	for id, at := range map[int64]time.Time{first: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC), second: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if _, err := db.conn.ExecContext(ctx, "UPDATE prompt_filter_logs SET created_at=$1 WHERE id=$2", at, id); err != nil {
			t.Fatal(err)
		}
	}
	mutations := []func(*PromptFilterLogInput){
		func(p *PromptFilterLogInput) { p.NewAPIUserID = "2"; p.NewAPIUserName = "Bob" },
		func(p *PromptFilterLogInput) { p.NewAPIPlatform = "other" },
		func(p *PromptFilterLogInput) { p.APIKeyID = 2 },
		func(p *PromptFilterLogInput) { p.Model = "another-model" },
		func(p *PromptFilterLogInput) { p.AuditScore = 25 },
		func(p *PromptFilterLogInput) { p.MatchContext = "different evidence" },
		func(p *PromptFilterLogInput) { p.PrimaryOrigin = "tool_output" },
		func(p *PromptFilterLogInput) { p.Action = "block" },
		func(p *PromptFilterLogInput) { p.ReviewFlagged = true },
	}
	for _, change := range mutations {
		input := base
		change(&input)
		insert(input)
	}
	logs, total, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Grouped: true, PageSize: 50})
	if err != nil || total != len(mutations)+1 || len(logs) != total {
		t.Fatalf("groups %d logs %d err %v", total, len(logs), err)
	}
	var repeated *PromptFilterLog
	for _, log := range logs {
		if log.OccurrenceCount == 2 {
			repeated = log
		}
	}
	if repeated == nil || repeated.ID != first || repeated.GroupID != second || repeated.FirstSeen == nil || repeated.LastSeen == nil || !repeated.LastSeen.After(*repeated.FirstSeen) {
		t.Fatalf("wrong representative: %+v", repeated)
	}
	for page := 1; page <= 2; page++ {
		items, count, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{GroupID: repeated.GroupID, Page: page, PageSize: 1})
		if err != nil || count != 2 || len(items) != 1 || items[0].OccurrenceCount != 0 {
			t.Fatalf("detail page %d: %+v %d %v", page, items, count, err)
		}
	}
	items, count, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{GroupID: second, SearchScope: "content", Query: "second user prompt"})
	if err != nil || count != 1 || len(items) != 1 || items[0].ID != second {
		t.Fatalf("filtered details: %d %+v %v", count, items, err)
	}
	_, count, err = db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{GroupID: second, SearchScope: "username", Query: "Bob"})
	if err != nil || count != 0 {
		t.Fatalf("cross-user filter: %d %v", count, err)
	}
	_, count, err = db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{GroupID: 999999})
	if err != nil || count != 0 {
		t.Fatalf("missing reference: %d %v", count, err)
	}
	items, count, err = db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Grouped: true, Sort: "audit_asc", PageSize: 1})
	if err != nil || count != total || len(items) != 1 || items[0].AuditScore != 25 {
		t.Fatalf("grouped sort: %d %+v %v", count, items, err)
	}
	seen := map[int64]bool{}
	for page := 1; page <= total; page++ {
		items, _, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Grouped: true, Page: page, PageSize: 1})
		if err != nil || len(items) != 1 {
			t.Fatalf("group page %d: %v", page, err)
		}
		if seen[items[0].GroupID] {
			t.Fatal("group repeated across pages")
		}
		seen[items[0].GroupID] = true
	}
	items, count, err = db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Grouped: true, Page: total + 1, PageSize: 1})
	if err != nil || count != total || len(items) != 0 {
		t.Fatalf("out-of-range page lost total: %d %v", count, err)
	}
	_, rawTotal, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{})
	if err != nil || rawTotal != len(mutations)+2 {
		t.Fatal(fmt.Sprintf("original records lost: %d %v", rawTotal, err))
	}
}

func BenchmarkPromptAuditGroups(b *testing.B) {
	db, err := New("sqlite", filepath.Join(b.TempDir(), "groups.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	tx, err := db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO prompt_filter_logs(source, newapi_user_id, api_key_id, audit_score, match_context) VALUES ('local_filter', $1, 1, 80, $2)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 250000; i++ {
		if _, err := stmt.Exec(fmt.Sprint(i%100), strings.Repeat("evidence ", 128)); err != nil {
			b.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, total, err := db.ListPromptFilterLogsPage(context.Background(), PromptFilterLogQuery{Grouped: true, Source: "local_filter", PageSize: 20})
		if err != nil || total != 100 {
			b.Fatalf("%d %v", total, err)
		}
	}
	b.StopTimer()
}

func TestPromptAuditGroupsIgnoreClearedReviewDetailsForAuxiliaryEvidence(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "review-groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	base := PromptFilterLogInput{Source: "local_filter", Action: "allow", PrimaryOrigin: "developer", APIKeyID: 1, NewAPIPlatform: "test", NewAPIUserID: "1", NewAPIPolicyStatus: "verified", AuditScore: 200, MatchContext: "same developer rule evidence", MatchedPatterns: `[{"name":"rule","weight":85}]`, Reviewed: true, ReviewModel: "omni-moderation-latest"}
	for i, prompt := range []string{"first user prompt", "second user prompt", "third user prompt"} {
		item := base
		item.TextPreview, item.FullText = prompt, prompt
		confidence, threshold := 0.01*float64(i+1), 0.8+0.05*float64(i)
		item.ReviewConfidence, item.ReviewThreshold = &confidence, &threshold
		item.ReviewReason = fmt.Sprintf("different category/reason %d", i)
		if err := db.InsertPromptFilterLog(ctx, &item); err != nil {
			t.Fatal(err)
		}
	}
	flagged := base
	flagged.ReviewFlagged = true
	flagged.Action = "block"
	if err := db.InsertPromptFilterLog(ctx, &flagged); err != nil {
		t.Fatal(err)
	}
	different := base
	different.MatchContext = "different developer evidence"
	if err := db.InsertPromptFilterLog(ctx, &different); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first direct request", "different direct request"} {
		item := base
		item.PrimaryOrigin = "current_user"
		item.TextPreview, item.FullText = prompt, prompt
		if err := db.InsertPromptFilterLog(ctx, &item); err != nil {
			t.Fatal(err)
		}
	}
	groups, total, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Grouped: true})
	if err != nil || total != 5 {
		t.Fatalf("want 5 groups, got %d: %v", total, err)
	}
	var groupID int64
	for _, group := range groups {
		if group.OccurrenceCount == 3 {
			groupID = group.GroupID
		}
	}
	if groupID == 0 {
		t.Fatalf("passing review details split identical auxiliary evidence: %+v", groups)
	}
	rows, count, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{GroupID: groupID})
	if err != nil || count != 3 || len(rows) != 3 {
		t.Fatalf("group details: %d %v", count, err)
	}
	for _, row := range rows {
		if row.ReviewConfidence == nil || row.ReviewReason == "" || row.FullText == "" {
			t.Fatal("grouping erased original details")
		}
	}
	rows, count, err = db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Grouped: true, SearchScope: "content", Query: "second user prompt"})
	if err != nil || count != 1 || len(rows) != 1 || rows[0].OccurrenceCount != 1 {
		t.Fatalf("prompt search no longer filters original rows: %d %v", count, err)
	}
}
