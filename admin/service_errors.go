package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func parseServiceErrorFilter(ctx *gin.Context, now time.Time) (database.ServiceErrorFilter, error) {
	filter := database.ServiceErrorFilter{Start: now.Add(-time.Hour), End: now, Limit: 20}
	start, end := ctx.Query("start"), ctx.Query("end")
	if start != "" || end != "" {
		var startError, endError error
		filter.Start, startError = time.Parse(time.RFC3339, start)
		filter.End, endError = time.Parse(time.RFC3339, end)
		if startError != nil || endError != nil || !filter.End.After(filter.Start) || filter.End.Sub(filter.Start) > 7*24*time.Hour {
			return filter, fmt.Errorf("start/end 必须是有效时间，查询范围不能超过 7 天")
		}
	}
	filter.Status, filter.Stage = ctx.Query("status"), ctx.Query("stage")
	switch filter.Status {
	case "", "429", "4xx", "5xx":
	default:
		return filter, fmt.Errorf("status 参数无效")
	}
	switch filter.Stage {
	case "", "authentication", "rate_limit", "root_binding", "window", "policy", "dispatch", "validation", "internal":
	default:
		return filter, fmt.Errorf("stage 参数无效")
	}
	filter.RequestID = strings.TrimSpace(ctx.Query("request_id"))
	if len(filter.RequestID) > 160 {
		return filter, fmt.Errorf("request_id 过长")
	}
	filter.Cursor = ctx.Query("cursor")
	if value := ctx.Query("grouped"); value != "" {
		if value != "true" && value != "false" {
			return filter, fmt.Errorf("grouped 必须为 true 或 false")
		}
		filter.Grouped = value == "true"
	}
	filter.GroupKey = ctx.Query("group_key")
	if !database.ValidateServiceErrorGroupKey(filter.GroupKey) || filter.Grouped && filter.GroupKey != "" {
		return filter, fmt.Errorf("group_key 参数无效；组内明细需要 grouped=false")
	}
	if !database.ValidateServiceErrorViewCursor(filter.Cursor, filter.Grouped) {
		return filter, fmt.Errorf("cursor 参数无效")
	}
	if value := ctx.Query("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			return filter, fmt.Errorf("limit 必须在 1 到 100 之间")
		}
		filter.Limit = limit
	}
	return filter, nil
}

func (handler *Handler) GetServiceErrorLogs(ctx *gin.Context) {
	filter, err := parseServiceErrorFilter(ctx, time.Now().UTC())
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if handler.db == nil {
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "服务错误日志存储暂不可用"})
		return
	}
	queryContext, cancel := context.WithTimeout(ctx.Request.Context(), 3*time.Second)
	defer cancel()
	page, err := handler.db.ListServiceErrors(queryContext, filter)
	if err != nil {
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "服务错误日志查询失败，请稍后重试"})
		return
	}
	for index := range page.Items {
		page.Items[index].UpstreamInfo = proxy.EnrichUpstreamWebsocketLifecycle(page.Items[index].UpstreamInfo)
	}
	ctx.JSON(http.StatusOK, page)
}
