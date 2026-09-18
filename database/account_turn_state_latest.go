package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AccountLatestTurnState 是账号页那一行「最近 turn-state」的一条读数。
type AccountLatestTurnState struct {
	CreatedAt         time.Time `json:"created_at"`
	TurnStateLength   *int      `json:"turn_state_length"`
	TurnStateEcho     string    `json:"turn_state_echo"`
	TurnStateStripped bool      `json:"turn_state_stripped"`
}

// GetAccountLatestTurnStates 返回每个账号最近一条终端用户请求的 turn-state 情况。
//
// 刻意只取「最近一条」，不往回找最近一条非空值：运营要看的是这个号现在还拿不拿得到
// turn-state，往回翻会把已经坏掉的号显示成健康的。NULL（未记录）与 0（检查过但上游
// 没给）原样透出，由前端分别显示「未记录」「未获取」。
//
// 空 ID 列表不发查询；超过批量上界（老的全量账号接口可能把整个号池传进来）整体跳过，
// 而不是对上万个账号各跑一次 LIMIT 1 子查询——这是个页面装饰字段，不值得拖垮列表。
func (db *DB) GetAccountLatestTurnStates(ctx context.Context, ids []int64, now time.Time) (map[int64]AccountLatestTurnState, error) {
	ids = positiveUniqueIDs(ids)
	result := make(map[int64]AccountLatestTurnState, len(ids))
	if len(ids) == 0 || len(ids) > accountRequestCountBreakdownMaxIDs {
		return result, nil
	}
	query, args := db.accountLatestTurnStateQuery(ids, now)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var created any
		var item AccountLatestTurnState
		if err := rows.Scan(&id, &created, &item.TurnStateLength, &item.TurnStateEcho, &item.TurnStateStripped); err != nil {
			return nil, err
		}
		item.CreatedAt, err = parseDBTimeValue(created)
		if err != nil {
			return nil, err
		}
		result[id] = item
	}
	return result, rows.Err()
}

func (db *DB) accountLatestTurnStateQuery(ids []int64, now time.Time) (string, []any) {
	// 每个账号一次有界查找，走 (account_id, created_at) 索引，id 给同批插入的行破平。
	// LIMIT 1 在查找内部，不是在全量聚合之后。PostgreSQL 用一个数组参数带整页 ID。
	if !db.isSQLite() {
		return `SELECT requested.account_id, latest.created_at, latest.turn_state_length,
				COALESCE(latest.turn_state_echo, ''), COALESCE(latest.turn_state_stripped, false)
			FROM unnest($1::bigint[]) AS requested(account_id)
			CROSS JOIN LATERAL (
				SELECT created_at, turn_state_length, turn_state_echo, turn_state_stripped FROM usage_logs
				WHERE account_id = requested.account_id AND created_at <= $2
				AND ` + db.endUserUsageLogPredicate() + `
				ORDER BY created_at DESC, id DESC LIMIT 1
			) AS latest`, []any{postgresInt8Array(ids), now}
	}
	args := []any{db.timeArg(now)}
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		values = append(values, fmt.Sprintf("($%d)", len(args)))
	}
	return `WITH requested(account_id) AS (VALUES ` + strings.Join(values, ",") + `)
		SELECT requested.account_id, latest.created_at, latest.turn_state_length,
			COALESCE(latest.turn_state_echo, ''), COALESCE(latest.turn_state_stripped, false)
		FROM requested JOIN usage_logs AS latest ON latest.id = (
			SELECT id FROM usage_logs
			WHERE account_id = requested.account_id AND created_at <= $1
			AND ` + db.endUserUsageLogPredicate() + `
			ORDER BY created_at DESC, id DESC LIMIT 1
		)`, args
}
