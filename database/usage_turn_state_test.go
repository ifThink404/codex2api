package database

import (
	"context"
	"testing"
	"time"
)

func newUsageTurnStateTestDB(t *testing.T) *DB {
	t.Helper()
	db := newGrokStateTestDB(t)
	db.SetUsageLogConfig(UsageLogModeFull, 1, 1)
	return db
}

// TestUsageLogTurnStateColumnsRoundTrip 走完整的落库链路（LogUsage → 缓冲 → flush →
// 列表查询），确认三列各自的三种形态都能原样回来。NULL 必须与 0 区分：0 是「检查过
// 上游响应，它没给 turn-state」，NULL 是「这条根本没记录」，运营读数完全不同。
func TestUsageLogTurnStateColumnsRoundTrip(t *testing.T) {
	db := newUsageTurnStateTestDB(t)
	ctx := context.Background()

	length292 := 292
	zero := 0
	rows := []*UsageLogInput{
		{AccountID: 11, Endpoint: "/v1/responses", Model: "gpt-6", StatusCode: 200,
			RequestID: "ts-recorded", TurnStateLength: &length292, TurnStateEcho: "cross", TurnStateStripped: true},
		{AccountID: 12, Endpoint: "/v1/responses", Model: "gpt-6", StatusCode: 200,
			RequestID: "ts-checked-empty", TurnStateLength: &zero, TurnStateEcho: "none", TurnStateStripped: false},
		{AccountID: 13, Endpoint: "/v1/responses", Model: "gpt-6", StatusCode: 200,
			RequestID: "ts-not-recorded"},
	}
	for _, row := range rows {
		if err := db.InsertUsageLog(context.Background(), row); err != nil {
			t.Fatalf("InsertUsageLog(%s): %v", row.RequestID, err)
		}
	}
	db.FlushUsageLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs: %v", err)
	}
	byRequest := make(map[string]*UsageLog, len(logs))
	for _, entry := range logs {
		byRequest[entry.RequestID] = entry
	}
	if len(byRequest) != 3 {
		t.Fatalf("persisted %d rows, want 3", len(byRequest))
	}

	recorded := byRequest["ts-recorded"]
	if recorded.TurnStateLength == nil || *recorded.TurnStateLength != 292 ||
		recorded.TurnStateEcho != "cross" || !recorded.TurnStateStripped {
		t.Fatalf("recorded row = %v/%q/%v, want 292/cross/true",
			recorded.TurnStateLength, recorded.TurnStateEcho, recorded.TurnStateStripped)
	}
	checkedEmpty := byRequest["ts-checked-empty"]
	if checkedEmpty.TurnStateLength == nil || *checkedEmpty.TurnStateLength != 0 ||
		checkedEmpty.TurnStateEcho != "none" || checkedEmpty.TurnStateStripped {
		t.Fatalf("checked-empty row = %v/%q/%v, want 0/none/false",
			checkedEmpty.TurnStateLength, checkedEmpty.TurnStateEcho, checkedEmpty.TurnStateStripped)
	}
	notRecorded := byRequest["ts-not-recorded"]
	if notRecorded.TurnStateLength != nil || notRecorded.TurnStateEcho != "" || notRecorded.TurnStateStripped {
		t.Fatalf("not-recorded row = %v/%q/%v, want nil/''/false",
			notRecorded.TurnStateLength, notRecorded.TurnStateEcho, notRecorded.TurnStateStripped)
	}
}

// TestUsageLogTurnStateEchoNormalization：回带分类是枚举列（VARCHAR(16)），来源是
// 网关自己的分类器，但落库前仍必须守住列宽与取值——一条超长脏值会让整批 INSERT
// 回滚，而失败的 batch 会被放回缓冲区头部反复重试，单条脏数据就能堵死日志写入。
func TestUsageLogTurnStateEchoNormalization(t *testing.T) {
	db := newUsageTurnStateTestDB(t)
	ctx := context.Background()

	negative := -5
	if err := db.InsertUsageLog(context.Background(), &UsageLogInput{
		AccountID: 21, Endpoint: "/v1/responses", StatusCode: 200, RequestID: "ts-dirty",
		TurnStateLength: &negative, TurnStateEcho: "this-class-is-far-too-long-to-store",
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	db.FlushUsageLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 5)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("persisted %d rows, want 1", len(logs))
	}
	if got := logs[0].TurnStateEcho; got != "" {
		t.Fatalf("TurnStateEcho = %q, want '' (unknown class dropped rather than stored)", got)
	}
	if logs[0].TurnStateLength == nil || *logs[0].TurnStateLength != 0 {
		t.Fatalf("TurnStateLength = %v, want 0 (negative clamped)", logs[0].TurnStateLength)
	}
}

// TestUsageLogTurnStateFilters 覆盖四个筛选参数在 WHERE 里的语义：
// received = 拿到了（>0）、missing = 检查过但没有（=0）、not_recorded = NULL。
func TestUsageLogTurnStateFilters(t *testing.T) {
	db := newUsageTurnStateTestDB(t)
	ctx := context.Background()

	length292, length312, zero := 292, 312, 0
	seed := []*UsageLogInput{
		{AccountID: 1, RequestID: "healthy", StatusCode: 200, TurnStateLength: &length292, TurnStateEcho: "none"},
		{AccountID: 2, RequestID: "degraded", StatusCode: 200, TurnStateLength: &length312, TurnStateEcho: "cross", TurnStateStripped: true},
		{AccountID: 3, RequestID: "checked-none", StatusCode: 200, TurnStateLength: &zero, TurnStateEcho: "same"},
		{AccountID: 4, RequestID: "legacy", StatusCode: 200},
	}
	for _, row := range seed {
		row.Endpoint = "/v1/responses"
		if err := db.InsertUsageLog(context.Background(), row); err != nil {
			t.Fatalf("InsertUsageLog(%s): %v", row.RequestID, err)
		}
	}
	db.FlushUsageLogs()

	stripped := true
	cases := []struct {
		name   string
		filter UsageLogFilter
		want   []string
	}{
		{"received", UsageLogFilter{TurnState: "received"}, []string{"degraded", "healthy"}},
		{"missing", UsageLogFilter{TurnState: "missing"}, []string{"checked-none"}},
		{"not_recorded", UsageLogFilter{TurnState: "not_recorded"}, []string{"legacy"}},
		{"length", UsageLogFilter{TurnStateLength: &length312}, []string{"degraded"}},
		{"echo", UsageLogFilter{TurnStateEcho: "cross"}, []string{"degraded"}},
		{"stripped", UsageLogFilter{TurnStateStripped: &stripped}, []string{"degraded"}},
	}
	start := time.Now().Add(-time.Hour)
	end := time.Now().Add(time.Hour)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := tc.filter
			filter.Start, filter.End = start, end
			filter.Page, filter.PageSize = 1, 50
			page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				t.Fatalf("ListUsageLogsByTimeRangePaged: %v", err)
			}
			got := make(map[string]bool, len(page.Logs))
			for _, entry := range page.Logs {
				got[entry.RequestID] = true
			}
			if len(got) != len(tc.want) {
				t.Fatalf("matched %v, want %v", got, tc.want)
			}
			for _, want := range tc.want {
				if !got[want] {
					t.Fatalf("matched %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestUsageLogTurnStateFilterDimensionKey：筛选必须进维度键，否则两种筛选组合会
// 共用同一条区间统计缓存，卡片数字与表格对不上。
func TestUsageLogTurnStateFilterDimensionKey(t *testing.T) {
	length := 312
	stripped := false
	cases := []UsageLogFilter{
		{TurnState: "received"},
		{TurnStateLength: &length},
		{TurnStateEcho: "cross"},
		{TurnStateStripped: &stripped},
	}
	seen := map[string]bool{}
	for _, filter := range cases {
		if !filter.HasDimensionFilter() {
			t.Fatalf("HasDimensionFilter() = false for %+v", filter)
		}
		key := filter.DimensionKey()
		if key == "" || seen[key] {
			t.Fatalf("DimensionKey() = %q for %+v (empty or colliding)", key, filter)
		}
		seen[key] = true
	}
	if (UsageLogFilter{}).HasDimensionFilter() {
		t.Fatal("empty filter must not report a dimension filter")
	}
}

// normalizeUsageLogTurnStateEcho 会静默丢弃超过 VARCHAR(16) 的分类。这条断言保证
// 已知的五个分类都装得下——否则某个合法分类会被当脏值丢掉，那一列永远是空的。
func TestUsageLogTurnStateEchoClassesFitTheColumn(t *testing.T) {
	for _, class := range UsageLogTurnStateEchoClasses {
		if len(class) > usageLogTurnStateEchoMax {
			t.Fatalf("turn_state_echo 分类 %q 有 %d 字节，超过列宽 %d", class, len(class), usageLogTurnStateEchoMax)
		}
		if got := normalizeUsageLogTurnStateEcho(class); got != class {
			t.Fatalf("normalizeUsageLogTurnStateEcho(%q) = %q, want it kept", class, got)
		}
	}
	// 白名单里没有、又超列宽的值一律丢弃，不截断。
	if got := normalizeUsageLogTurnStateEcho("substitute-expired-and-then-some"); got != "" {
		t.Fatalf("over-wide class = %q, want '' (dropped, never truncated)", got)
	}
}
