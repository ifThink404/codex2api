package database

import (
	"context"
	"os"
	"testing"
	"time"
)

// Run against an empty disposable database, as for the other PostgreSQL tests:
//
//	docker run -d --rm --name c2a-pg-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=codex2api_test -p 55432:5432 postgres:16
//	CODEX2API_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/codex2api_test?sslmode=disable' \
//	    go test ./database/ -run TestPostgresUsageTurnState -count=1
//
// 生产跑的是 PostgreSQL，而 turn-state 三列只在 SQLite 上测过。这里覆盖只有 pq
// 驱动才算数的几处：老库升级时三列的 ADD COLUMN 回填与默认值、68 个占位符的批量
// INSERT（列/值错位在 SQLite 上也可能蒙混过关）、VARCHAR(16) 的列宽、四个筛选
// 在 PostgreSQL 布尔语义下的 NULL 行为，以及账号最近 turn-state 的
// `unnest + LATERAL LIMIT 1` 查询（SQLite 走的是另一套相关子查询）。
func TestPostgresUsageTurnState(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(UsageLogModeFull, 1, 1)
	ctx := context.Background()

	if _, err := db.conn.ExecContext(ctx, `DELETE FROM usage_logs`); err != nil {
		t.Fatalf("reset usage_logs: %v", err)
	}

	t.Run("column backfill", func(t *testing.T) { postgresUsageTurnStateColumnBackfill(ctx, t, db) })
	t.Run("round trip and filters", func(t *testing.T) { postgresUsageTurnStateRoundTrip(ctx, t, db) })
	t.Run("account latest", func(t *testing.T) { postgresUsageTurnStateAccountLatest(ctx, t, db) })
}

// postgresUsageTurnStateColumnBackfill 走老库升级那条路：三列不存在的既有行经过
// migrate() 之后必须拿到 NULL / ” / FALSE。长度列尤其不能有 DEFAULT 0——历史行
// 必须留在「未记录」，否则整张旧表会被显示成「上游从没给过 turn-state」。
func postgresUsageTurnStateColumnBackfill(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	for _, column := range []string{"turn_state_length", "turn_state_echo", "turn_state_stripped"} {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE usage_logs DROP COLUMN IF EXISTS `+column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if _, err := db.conn.ExecContext(ctx,
		`INSERT INTO usage_logs (account_id, endpoint, status_code, request_id) VALUES (901, '/v1/responses', 200, 'pg-legacy')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatalf("migrate must re-add the three columns: %v", err)
	}
	var length *int
	var echo string
	var stripped bool
	if err := db.conn.QueryRowContext(ctx,
		`SELECT turn_state_length, turn_state_echo, turn_state_stripped FROM usage_logs WHERE request_id = 'pg-legacy'`).
		Scan(&length, &echo, &stripped); err != nil {
		t.Fatalf("read back the migrated row: %v", err)
	}
	if length != nil || echo != "" || stripped {
		t.Fatalf("migrated legacy row = %v/%q/%v, want NULL/''/false", length, echo, stripped)
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM usage_logs`); err != nil {
		t.Fatalf("reset usage_logs: %v", err)
	}
}

// postgresUsageTurnStateMarker 把本用例的行与同库里其它用例的行隔开。
const postgresUsageTurnStateMarker = "pg-ts-"

func postgresUsageTurnStateRoundTrip(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	length292, length312, zero := 292, 312, 0
	seed := []*UsageLogInput{
		{AccountID: 911, RequestID: "pg-ts-healthy", StatusCode: 200, TurnStateLength: &length292, TurnStateEcho: "none"},
		{AccountID: 912, RequestID: "pg-ts-degraded", StatusCode: 200, TurnStateLength: &length312, TurnStateEcho: "cross", TurnStateStripped: true},
		{AccountID: 913, RequestID: "pg-ts-checked-none", StatusCode: 200, TurnStateLength: &zero, TurnStateEcho: "substitute"},
		{AccountID: 914, RequestID: "pg-ts-legacy-row", StatusCode: 200},
	}
	for _, row := range seed {
		row.Endpoint = "/v1/responses"
		if err := db.InsertUsageLog(context.Background(), row); err != nil {
			t.Fatalf("InsertUsageLog(%s): %v", row.RequestID, err)
		}
	}
	db.FlushUsageLogs()

	// 同一个库里别的 PostgreSQL 用例也在写 usage_logs（有的还带着未关闭的异步刷盘），
	// 所以按「最近 N 条」读回会被串扰。统一按本用例的 request_id 前缀取。
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start: start, End: end, Page: 1, PageSize: 200, Query: postgresUsageTurnStateMarker,
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged: %v", err)
	}
	byRequest := make(map[string]*UsageLog, len(page.Logs))
	for _, entry := range page.Logs {
		byRequest[entry.RequestID] = entry
	}
	if row := byRequest["pg-ts-degraded"]; row == nil || row.TurnStateLength == nil || *row.TurnStateLength != 312 ||
		row.TurnStateEcho != "cross" || !row.TurnStateStripped {
		t.Fatalf("pg-degraded = %+v, want 312/cross/true", row)
	}
	if row := byRequest["pg-ts-checked-none"]; row == nil || row.TurnStateLength == nil || *row.TurnStateLength != 0 ||
		row.TurnStateEcho != "substitute" {
		t.Fatalf("pg-checked-none = %+v, want 0/substitute", row)
	}
	if row := byRequest["pg-ts-legacy-row"]; row == nil || row.TurnStateLength != nil || row.TurnStateEcho != "" {
		t.Fatalf("pg-legacy-row = %+v, want NULL/''", row)
	}

	stripped := true
	cases := []struct {
		name   string
		filter UsageLogFilter
		want   string
	}{
		{"received", UsageLogFilter{TurnState: "received"}, "pg-ts-healthy,pg-ts-degraded"},
		{"missing", UsageLogFilter{TurnState: "missing"}, "pg-ts-checked-none"},
		{"not_recorded", UsageLogFilter{TurnState: "not_recorded"}, "pg-ts-legacy-row"},
		{"length", UsageLogFilter{TurnStateLength: &length312}, "pg-ts-degraded"},
		{"echo", UsageLogFilter{TurnStateEcho: "substitute"}, "pg-ts-checked-none"},
		{"stripped", UsageLogFilter{TurnStateStripped: &stripped}, "pg-ts-degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := tc.filter
			filter.Start, filter.End = start, end
			filter.Page, filter.PageSize = 1, 200
			filter.Query = postgresUsageTurnStateMarker
			page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				t.Fatalf("ListUsageLogsByTimeRangePaged: %v", err)
			}
			got := map[string]bool{}
			for _, entry := range page.Logs {
				got[entry.RequestID] = true
			}
			want := map[string]bool{}
			for _, id := range splitCommaList(tc.want) {
				want[id] = true
			}
			if len(got) != len(want) {
				t.Fatalf("matched %v, want %v", got, want)
			}
			for id := range want {
				if !got[id] {
					t.Fatalf("matched %v, want %v", got, want)
				}
			}
		})
	}
}

func postgresUsageTurnStateAccountLatest(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	now := time.Now().UTC()
	length292, length312 := 292, 312
	insert := func(accountID int64, at time.Time, value *int, echo string, stripped bool, reason string) {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
			(account_id, status_code, internal_reason, turn_state_length, turn_state_echo, turn_state_stripped, created_at)
			VALUES ($1, 200, $2, $3, $4, $5, $6)`,
			accountID, reason, value, echo, stripped, at); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}
	insert(921, now.Add(-10*time.Minute), &length292, "none", false, "")
	insert(921, now.Add(-time.Minute), &length312, "cross", true, "")
	insert(922, now.Add(-10*time.Minute), &length292, "none", false, "")
	insert(922, now.Add(-time.Minute), nil, "", false, "")
	insert(923, now.Add(-time.Minute), &length312, "cross", true, "grok_capability_probe")

	latest, err := db.GetAccountLatestTurnStates(ctx, []int64{921, 922, 923, 924}, now)
	if err != nil {
		t.Fatalf("GetAccountLatestTurnStates: %v", err)
	}
	if row, ok := latest[921]; !ok || row.TurnStateLength == nil || *row.TurnStateLength != 312 ||
		row.TurnStateEcho != "cross" || !row.TurnStateStripped || row.CreatedAt.IsZero() {
		t.Fatalf("account 921 = %+v, want the newest row 312/cross/stripped", row)
	}
	if row, ok := latest[922]; !ok || row.TurnStateLength != nil {
		t.Fatalf("account 922 = %+v, want NULL length preserved", row)
	}
	if _, ok := latest[923]; ok {
		t.Fatal("account 923 only has internal traffic and must be absent")
	}
	if _, ok := latest[924]; ok {
		t.Fatal("account 924 has no rows and must be absent")
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM usage_logs`); err != nil {
		t.Fatalf("reset usage_logs: %v", err)
	}
}

func splitCommaList(value string) []string {
	out := []string{}
	current := ""
	for _, r := range value {
		if r == ',' {
			if current != "" {
				out = append(out, current)
			}
			current = ""
			continue
		}
		current += string(r)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}
