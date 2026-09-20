package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Run against an empty disposable database, as for the other PostgreSQL tests:
//
//	docker run -d --rm --name c2a-pg-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=codex2api_test -p 55432:5432 postgres:16
//	CODEX2API_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/codex2api_test?sslmode=disable' \
//	    go test ./database/ -run TestPostgresUsageLogUpstreamModelAndWindowNumber -count=1
//
// 生产跑的是 PostgreSQL，而 usage_logs 的两条写路径在两种驱动上完全不同：SQLite 走
// 逐行 prepared statement，PostgreSQL 走 65 列 * N 行的批量 VALUES。新增列时只要
// 列清单和取值清单有一处没同步，SQLite 那边照样绿，PostgreSQL 这边才会以类型错误
// 或者「值写进了相邻列」的形式暴露出来。
func TestPostgresUsageLogUpstreamModelAndWindowNumber(t *testing.T) {
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

	t.Run("batch insert round-trip", func(t *testing.T) { postgresUsageLogNewColumnsRoundTrip(ctx, t, db) })
	t.Run("column backfill", func(t *testing.T) { postgresUsageLogNewColumnsBackfill(ctx, t, db) })
	t.Run("column widths", func(t *testing.T) { postgresUsageLogNewColumnWidths(ctx, t, db) })
}

// postgresUsageLogNewColumnsRoundTrip 走完整写路径：缓冲 → 批量 INSERT → 三条读路径。
// 多塞几行是刻意的：批量 VALUES 每行自带一组占位符，单行写对不代表 argIdx 步进对。
func postgresUsageLogNewColumnsRoundTrip(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	const rows = 3
	marker := fmt.Sprintf("pg-usage-model-%d", time.Now().UnixNano())
	for i := 0; i < rows; i++ {
		if err := db.InsertUsageLog(ctx, &UsageLogInput{
			RequestID:             fmt.Sprintf("%s-%d", marker, i),
			Endpoint:              "/v1/responses",
			InboundEndpoint:       marker,
			Model:                 "gpt-5.4",
			EffectiveModel:        "gpt-5.4",
			UpstreamResponseModel: fmt.Sprintf("gpt-5.4-codex-%d", i),
			WindowNumber:          fmt.Sprintf("%d", i+1),
			ReasoningEffort:       "high",
			StatusCode:            200,
			TotalTokens:           10,
		}); err != nil {
			t.Fatalf("InsertUsageLog(%d): %v", i, err)
		}
	}
	db.FlushUsageLogs()

	// 先直接读列，绕开读路径上的 COALESCE：值真的进了这两列，而不是被读侧补出来的。
	for i := 0; i < rows; i++ {
		var model, window string
		if err := db.conn.QueryRowContext(ctx,
			`SELECT upstream_response_model, window_number FROM usage_logs WHERE request_id = $1`,
			fmt.Sprintf("%s-%d", marker, i)).Scan(&model, &window); err != nil {
			t.Fatalf("read row %d: %v", i, err)
		}
		if model != fmt.Sprintf("gpt-5.4-codex-%d", i) || window != fmt.Sprintf("%d", i+1) {
			t.Fatalf("row %d = (%q, %q); the batch INSERT column list is misaligned", i, model, window)
		}
	}

	window := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Page: 1, PageSize: 50, Endpoint: marker}
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, window)
	if err != nil || page == nil || len(page.Logs) != rows {
		t.Fatalf("ListUsageLogsByTimeRangePaged = %#v, err = %v", page, err)
	}
	exported, err := db.ListUsageLogsByFilter(ctx, window)
	if err != nil || len(exported) != rows {
		t.Fatalf("ListUsageLogsByFilter = %d rows, err = %v", len(exported), err)
	}
	for name, logs := range map[string][]*UsageLog{"paged": page.Logs, "export": exported} {
		for _, row := range logs {
			if !strings.HasPrefix(row.UpstreamResponseModel, "gpt-5.4-codex-") || row.WindowNumber == "" {
				t.Fatalf("%s: upstream_response_model=%q window_number=%q", name, row.UpstreamResponseModel, row.WindowNumber)
			}
			// 相邻列读串在 Scan 层不会报错，只会安静地错位。
			if row.ReasoningEffort != "high" || row.Model != "gpt-5.4" {
				t.Fatalf("%s: neighbouring columns read out of order: reasoning_effort=%q model=%q", name, row.ReasoningEffort, row.Model)
			}
		}
	}
}

// postgresUsageLogNewColumnsBackfill 走老库升级那条路：两列不存在的既有行经过
// migrate() 之后必须拿到空串，而不是 NULL——NULL 会让不带 COALESCE 的新查询炸掉。
func postgresUsageLogNewColumnsBackfill(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	marker := fmt.Sprintf("pg-usage-model-legacy-%d", time.Now().UnixNano())
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		RequestID: marker, Endpoint: "/v1/responses", InboundEndpoint: marker,
		Model: "gpt-5.4", StatusCode: 200,
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	db.FlushUsageLogs()

	for _, column := range []string{"upstream_response_model", "window_number"} {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE usage_logs DROP COLUMN IF EXISTS `+column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatalf("migrate must re-add both columns: %v", err)
	}

	var model, windowNumber *string
	if err := db.conn.QueryRowContext(ctx,
		`SELECT upstream_response_model, window_number FROM usage_logs WHERE request_id = $1`, marker).
		Scan(&model, &windowNumber); err != nil {
		t.Fatalf("read the backfilled columns: %v", err)
	}
	if model == nil || *model != "" || windowNumber == nil || *windowNumber != "" {
		t.Fatalf("ADD COLUMN ... DEFAULT '' must backfill existing rows with empty strings, got %v / %v", model, windowNumber)
	}
}

// postgresUsageLogNewColumnWidths 锁住两列的宽度：VARCHAR(200) / VARCHAR(32) 在
// PostgreSQL 上按字符计。写入侧的截断上限就是照这两个列宽定的，超一个字符整条
// 批量 INSERT 会回滚，失败批次又被原样放回缓冲头部，一条脏数据堵死整条日志写入。
func postgresUsageLogNewColumnWidths(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	marker := fmt.Sprintf("pg-usage-model-wide-%d", time.Now().UnixNano())
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		RequestID: marker, Endpoint: "/v1/responses", InboundEndpoint: marker, Model: "gpt-5.4",
		UpstreamResponseModel: strings.Repeat("模", 300),
		WindowNumber:          strings.Repeat("9", 300),
		StatusCode:            200,
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	db.FlushUsageLogs()

	var model, windowNumber string
	if err := db.conn.QueryRowContext(ctx,
		`SELECT upstream_response_model, window_number FROM usage_logs WHERE request_id = $1`, marker).
		Scan(&model, &windowNumber); err != nil {
		t.Fatalf("an over-long value must be clamped, not rejected: %v", err)
	}
	if got := len([]rune(model)); got != upstreamResponseModelMaxLen {
		t.Fatalf("upstream_response_model kept %d runes, want %d", got, upstreamResponseModelMaxLen)
	}
	if got := len([]rune(windowNumber)); got != usageLogWindowNumberMaxLen {
		t.Fatalf("window_number kept %d runes, want %d", got, usageLogWindowNumberMaxLen)
	}

	// 绕开写入侧截断直接写宽值：列宽约束必须真的落在 PostgreSQL 上。
	if _, err := db.conn.ExecContext(ctx,
		`INSERT INTO usage_logs (endpoint, model, upstream_response_model) VALUES ('/v1/responses', 'gpt-5.4', $1)`,
		strings.Repeat("x", upstreamResponseModelMaxLen+1)); err == nil {
		t.Fatalf("upstream_response_model must be VARCHAR(%d)", upstreamResponseModelMaxLen)
	}
	if _, err := db.conn.ExecContext(ctx,
		`INSERT INTO usage_logs (endpoint, model, window_number) VALUES ('/v1/responses', 'gpt-5.4', $1)`,
		strings.Repeat("9", usageLogWindowNumberMaxLen+1)); err == nil {
		t.Fatalf("window_number must be VARCHAR(%d)", usageLogWindowNumberMaxLen)
	}
}
