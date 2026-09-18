package database

import (
	"context"
	"os"
	"testing"
)

// Run against an empty disposable database, as for the other PostgreSQL tests:
//
//	docker run -d --rm --name c2a-pg-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=codex2api_test -p 55432:5432 postgres:16
//	CODEX2API_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/codex2api_test?sslmode=disable' \
//	    go test ./database/ -run TestPostgresPromptFilterLogAccountID -count=1
//
// 生产跑的是 PostgreSQL。prompt_filter_logs.account_id 是可空列：存量行保持 NULL，
// 读取侧靠 COALESCE 归 0。SQLite 的弱类型会把列位错配吞掉，pq 不会，所以 INSERT
// 与 SELECT/Scan 的列位对齐必须在 pq 上验一遍。
func TestPostgresPromptFilterLogAccountID(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	var dataType, isNullable string
	if err := db.conn.QueryRowContext(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_name = 'prompt_filter_logs' AND column_name = 'account_id'`).Scan(&dataType, &isNullable); err != nil {
		t.Fatalf("describe account_id: %v", err)
	}
	if dataType != "bigint" || isNullable != "YES" {
		t.Fatalf("account_id = %s nullable=%s, want bigint/YES", dataType, isNullable)
	}

	if err := db.ClearPromptFilterLogs(ctx); err != nil {
		t.Fatalf("ClearPromptFilterLogs: %v", err)
	}
	t.Cleanup(func() { _ = db.ClearPromptFilterLogs(ctx) })

	// 存量行（升级前写入，account_id 为 NULL）必须能读回 0 而不是扫描报错。
	if _, err := db.conn.ExecContext(ctx,
		`INSERT INTO prompt_filter_logs (source, endpoint, action) VALUES ('local_filter', '/v1/responses', 'block')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "account_exempt", Endpoint: "/v1/responses", Model: "gpt-5.5",
		Action: "block", AccountID: 4242, TextPreview: "bounded prompt",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}

	logs, total, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListPromptFilterLogsPage: %v", err)
	}
	if total != 2 || len(logs) != 2 {
		t.Fatalf("rows total=%d len=%d, want 2/2", total, len(logs))
	}
	bySource := map[string]*PromptFilterLog{}
	for _, item := range logs {
		bySource[item.Source] = item
	}
	exempt := bySource["account_exempt"]
	if exempt == nil || exempt.AccountID != 4242 {
		t.Fatalf("account_exempt row = %+v, want account_id 4242", exempt)
	}
	legacy := bySource["local_filter"]
	if legacy == nil || legacy.AccountID != 0 {
		t.Fatalf("legacy NULL row = %+v, want account_id 0", legacy)
	}
}
