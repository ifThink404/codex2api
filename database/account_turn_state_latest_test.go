package database

import (
	"context"
	"testing"
	"time"
)

// TestGetAccountLatestTurnStates 是账号页那一行「最近 turn-state」的取数：每个账号
// 只取最近一条终端用户请求，不往回找「最近一条非空值」——运营要看的正是「这个号
// 现在到底还拿不拿得到 turn-state」，往回翻会把已经坏掉的号显示成健康的。
func TestGetAccountLatestTurnStates(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	insert := func(accountID int64, at time.Time, length *int, echo string, stripped bool, internalReason string) {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
			(account_id, status_code, internal_reason, turn_state_length, turn_state_echo, turn_state_stripped, created_at)
			VALUES ($1, 200, $2, $3, $4, $5, $6)`,
			accountID, internalReason, length, echo, stripped, sqliteTimeParam(at)); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}
	length292, length312, zero := 292, 312, 0

	// 账号 1：更新的那条才算数。
	insert(1, now.Add(-10*time.Minute), &length292, "none", false, "")
	insert(1, now.Add(-time.Minute), &length312, "cross", true, "")
	// 账号 2：最近一条是「检查过但上游没给」，必须显示 0 而不是回退到更早的 292。
	insert(2, now.Add(-10*time.Minute), &length292, "none", false, "")
	insert(2, now.Add(-time.Minute), &zero, "none", false, "")
	// 账号 3：最近一条是历史行（NULL），NULL 必须原样保留。
	insert(3, now.Add(-10*time.Minute), &length292, "same", false, "")
	insert(3, now.Add(-time.Minute), nil, "", false, "")
	// 账号 4：最近一条是网关内部流量，必须被 endUserUsageLogPredicate 排除。
	insert(4, now.Add(-10*time.Minute), &length292, "none", false, "")
	insert(4, now.Add(-time.Minute), &length312, "cross", true, "grok_capability_probe")
	// 账号 5：只有未来的行，超出 now 的不算。
	insert(5, now.Add(time.Hour), &length312, "cross", true, "")

	latest, err := db.GetAccountLatestTurnStates(ctx, []int64{1, 2, 3, 4, 5, 6}, now)
	if err != nil {
		t.Fatalf("GetAccountLatestTurnStates: %v", err)
	}

	first, ok := latest[1]
	if !ok || first.TurnStateLength == nil || *first.TurnStateLength != 312 ||
		first.TurnStateEcho != "cross" || !first.TurnStateStripped {
		t.Fatalf("account 1 = %+v, want the newest row 312/cross/stripped", first)
	}
	if first.CreatedAt.IsZero() {
		t.Fatal("account 1 CreatedAt must be populated")
	}
	second, ok := latest[2]
	if !ok || second.TurnStateLength == nil || *second.TurnStateLength != 0 {
		t.Fatalf("account 2 = %+v, want an explicit zero length", second)
	}
	third, ok := latest[3]
	if !ok || third.TurnStateLength != nil || third.TurnStateEcho != "" {
		t.Fatalf("account 3 = %+v, want NULL length preserved", third)
	}
	fourth, ok := latest[4]
	if !ok || fourth.TurnStateLength == nil || *fourth.TurnStateLength != 292 {
		t.Fatalf("account 4 = %+v, want the internal-traffic row ignored", fourth)
	}
	if _, ok := latest[5]; ok {
		t.Fatal("account 5 has no row at or before now and must be absent")
	}
	if _, ok := latest[6]; ok {
		t.Fatal("account 6 has no rows at all and must be absent")
	}
}

func TestGetAccountLatestTurnStatesEmptyAndOversizedIDs(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()

	result, err := db.GetAccountLatestTurnStates(ctx, nil, time.Now())
	if err != nil || len(result) != 0 {
		t.Fatalf("empty ids = %v, err %v; want an empty map and no query", result, err)
	}

	// 老的全量账号接口可能传进整个号池；批量取数有上界，超过就整体跳过，
	// 而不是对 40k 个账号各跑一次 LATERAL 子查询。
	tooMany := make([]int64, accountRequestCountBreakdownMaxIDs+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	result, err = db.GetAccountLatestTurnStates(ctx, tooMany, time.Now())
	if err != nil || len(result) != 0 {
		t.Fatalf("oversized ids = %v, err %v; want an empty map", result, err)
	}
}
