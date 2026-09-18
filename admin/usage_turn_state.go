package admin

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 用量页与账号页的 turn-state 读数。长度是账号级的「降智桶」标记（线上实测同一时刻
// 健康号 292 字符、其余号 312 字符），按账号看 turn_state_echo=cross 或
// turn_state_stripped=true 的分布就是「降智账号是否与外来 turn-state 相关」的直接答案。

// parseUsageTurnStateFilters 解析 turn_state / turn_state_length / turn_state_echo /
// turn_state_stripped 四个参数。取值非法一律 400，不静默忽略：忽略之后页面会显示
// 「0 条」，运营会把它读成「没有这类请求」，而不是「参数写错了」。
func parseUsageTurnStateFilters(c *gin.Context, filter *database.UsageLogFilter) bool {
	filter.TurnState = strings.ToLower(strings.TrimSpace(c.Query("turn_state")))
	switch filter.TurnState {
	case "", "received", "missing", "not_recorded":
	default:
		writeError(c, http.StatusBadRequest, "turn_state 参数无效，需要 received/missing/not_recorded")
		return false
	}

	if raw := strings.TrimSpace(c.Query("turn_state_length")); raw != "" {
		length, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || length < 0 {
			writeError(c, http.StatusBadRequest, "turn_state_length 参数无效，需要非负整数")
			return false
		}
		value := int(length)
		filter.TurnStateLength = &value
	}

	if raw := strings.TrimSpace(c.Query("turn_state_echo")); raw != "" {
		echo := strings.ToLower(raw)
		if !isUsageTurnStateEchoClass(echo) {
			writeError(c, http.StatusBadRequest, "turn_state_echo 参数无效，需要 "+strings.Join(database.UsageLogTurnStateEchoClasses, "/"))
			return false
		}
		filter.TurnStateEcho = echo
	}

	stripped, ok := parseUsageLogBoolFilter(c, "turn_state_stripped")
	if !ok {
		return false
	}
	filter.TurnStateStripped = stripped
	return true
}

func isUsageTurnStateEchoClass(value string) bool {
	for _, allowed := range database.UsageLogTurnStateEchoClasses {
		if value == allowed {
			return true
		}
	}
	return false
}

// accountLatestTurnStateResponse 是账号行上的「最近 turn-state」。长度为 null 表示
// 未记录（历史行 / 非官方路径），0 表示检查过上游但它没给——前端分别显示
// 「未记录」「未获取」，两者不能合并。
type accountLatestTurnStateResponse struct {
	CreatedAt string `json:"created_at"`
	Length    *int   `json:"turn_state_length"`
	Echo      string `json:"turn_state_echo"`
	Stripped  bool   `json:"turn_state_stripped"`
}

// attachAccountLatestTurnStates 按当页账号 ID 批量取一次，挂到对应的行上。
// 只查当页：这是个展示字段，不值得为它对整个号池跑一遍逐账号查找。失败只记日志、
// 不影响账号列表本身返回。
func (h *Handler) attachAccountLatestTurnStates(ctx context.Context, accounts []accountResponse) {
	if h == nil || h.db == nil || len(accounts) == 0 {
		return
	}
	ids := make([]int64, 0, len(accounts))
	for i := range accounts {
		if accounts[i].ID > 0 {
			ids = append(ids, accounts[i].ID)
		}
	}
	latest, err := h.db.GetAccountLatestTurnStates(ctx, ids, time.Now())
	if err != nil {
		log.Printf("批量获取账号最近 turn-state 失败: %v", err)
		return
	}
	if len(latest) == 0 {
		return
	}
	for i := range accounts {
		row, ok := latest[accounts[i].ID]
		if !ok {
			continue
		}
		accounts[i].LatestTurnState = &accountLatestTurnStateResponse{
			CreatedAt: row.CreatedAt.Format(time.RFC3339),
			Length:    row.TurnStateLength,
			Echo:      row.TurnStateEcho,
			Stripped:  row.TurnStateStripped,
		}
	}
}
