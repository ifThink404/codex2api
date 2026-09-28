package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPromptAuditSortAndUsernameScope(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, input := range []PromptFilterLogInput{
		{Source: "local_filter", Action: "allow", AuditScore: 80, NewAPIPlatform: "site-a", NewAPIUserID: "1", NewAPIUserName: "Alice_100%", TextPreview: "first"},
		{Source: "local_filter", Action: "allow", AuditScore: 25, NewAPIPlatform: "site-a", NewAPIUserID: "2", NewAPIUserName: "Bob", TextPreview: "Alice_100%"},
		{Source: "local_filter", Action: "allow", AuditScore: 80, NewAPIPlatform: "site-b", NewAPIUserID: "1", NewAPIUserName: "Carol", TextPreview: "third"},
	} {
		input.NewAPIPolicyStatus = "verified"
		if err := db.InsertPromptFilterLog(ctx, &input); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		scope, q string
		total    int
	}{
		{"username", "alice_100%", 1}, {"all", "alice_100%", 2}, {"content", "alice_100%", 1},
		{"username", "%", 1}, {"username", "_", 1}, {"username", "carol", 1}, {"rules", "Alice", 0},
	} {
		logs, total, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{SearchScope: tc.scope, Query: tc.q, PageSize: 10})
		if err != nil || total != tc.total || len(logs) != tc.total {
			t.Fatalf("%+v: total=%d logs=%d err=%v", tc, total, len(logs), err)
		}
	}
	var ids []int64
	for page := 1; page <= 3; page++ {
		logs, total, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Sort: "audit_desc", Page: page, PageSize: 1})
		if err != nil || total != 3 || len(logs) != 1 {
			t.Fatalf("page %d total=%d err=%v", page, total, err)
		}
		ids = append(ids, logs[0].ID)
		if page < 3 && logs[0].AuditScore != 80 {
			t.Fatal("sort applied after pagination")
		}
		if page == 3 && logs[0].AuditScore != 25 {
			t.Fatal("wrong low score")
		}
	}
	if ids[0] <= ids[1] || ids[1] == ids[2] {
		t.Fatalf("unstable order: %v", ids)
	}
	logs, _, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Sort: "audit_asc", PageSize: 1})
	if err != nil || len(logs) != 1 || logs[0].AuditScore != 25 {
		t.Fatalf("ascending: %+v %v", logs, err)
	}
}
