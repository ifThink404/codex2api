package admin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/logagent"
)

const (
	// logAgentSourceUsageLogs：Refs 为请求 ID，取出该请求的全部尝试（含成功的重试）。
	logAgentSourceUsageLogs = "usage_logs"
	// logAgentSourceOpsErrors：运维错误页的筛选条件（与 /ops/errors 同名参数），只取错误行。
	logAgentSourceOpsErrors = "ops_errors"

	logAgentDefaultFilterWindow = time.Hour
	logAgentDefaultRefWindow    = 7 * 24 * time.Hour
	logAgentMaxWindow           = 31 * 24 * time.Hour
	logAgentMaxFilterPage       = 500
)

type usageLogAgentSource struct {
	db        *database.DB
	name      string
	errorOnly bool
}

func (s *usageLogAgentSource) Name() string { return s.name }

func (s *usageLogAgentSource) Fetch(ctx context.Context, q logagent.Query) ([]logagent.Record, error) {
	if s.db == nil {
		return nil, fmt.Errorf("数据库不可用")
	}
	start, end := q.Start, q.End
	if end.IsZero() {
		end = time.Now()
	}
	if start.IsZero() {
		window := logAgentDefaultFilterWindow
		if len(q.Refs) > 0 {
			window = logAgentDefaultRefWindow
		}
		start = end.Add(-window)
	}
	if end.Sub(start) > logAgentMaxWindow {
		start = end.Add(-logAgentMaxWindow)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = logagent.DefaultMaxRecords
	}

	var logs []*database.UsageLog
	if len(q.Refs) > 0 {
		for _, ref := range q.Refs {
			rows, err := s.db.ListUsageLogsByFilter(ctx, database.UsageLogFilter{
				Start: start, End: end, RequestID: ref, ErrorOnly: s.errorOnly, IncludeCanceled: true,
			})
			if err != nil {
				return nil, err
			}
			logs = append(logs, rows...)
			if len(logs) >= limit {
				logs = logs[:limit]
				break
			}
		}
	} else {
		filter, err := usageLogFilterFromLogAgentFilters(q.Filters)
		if err != nil {
			return nil, err
		}
		filter.Start, filter.End = start, end
		filter.ErrorOnly = filter.ErrorOnly || s.errorOnly
		filter.IncludeCanceled = true
		filter.Page, filter.PageSize = 1, min(limit, logAgentMaxFilterPage)
		page, err := s.db.ListUsageLogsByTimeRangePaged(ctx, filter)
		if err != nil {
			return nil, err
		}
		logs = page.Logs
	}
	records := make([]logagent.Record, 0, len(logs))
	for _, row := range logs {
		if row != nil {
			records = append(records, usageLogAgentRecord(row))
		}
	}
	return records, nil
}

// usageLogFilterFromLogAgentFilters 接受与 /ops/errors 相同的查询参数名，
// 使运维错误页可以把当前筛选条件原样交给分析。
func usageLogFilterFromLogAgentFilters(filters map[string]string) (database.UsageLogFilter, error) {
	var filter database.UsageLogFilter
	for key, value := range filters {
		switch key {
		case "email":
			filter.Email = value
		case "model":
			filter.Model = value
		case "endpoint":
			filter.Endpoint = value
		case "error_kind":
			filter.ErrorKind = value
		case "q":
			filter.Query = value
		case "request_id":
			filter.RequestID = value
		case "transport":
			filter.Transport = value
		case "channel":
			switch value {
			case database.UpstreamChannelCodex, database.UpstreamChannelGrok, database.UpstreamChannelAntigravity, database.UpstreamChannelClaude:
				filter.Channel = value
			}
		case "api_key_id", "account_id":
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				return filter, fmt.Errorf("%s 参数无效", key)
			}
			if key == "api_key_id" {
				filter.APIKeyID = &id
			} else {
				filter.AccountID = &id
			}
		case "status", "status_code":
			switch strings.ToLower(value) {
			case "all":
			case "4xx", "5xx":
				filter.StatusFamily = strings.ToLower(value)
			default:
				code, err := strconv.Atoi(value)
				if err != nil || code < 100 || code > 599 {
					return filter, fmt.Errorf("status 参数无效")
				}
				filter.StatusCode = code
			}
		case "error_only":
			filter.ErrorOnly = value == "true"
		case "stream":
			stream := value == "true"
			filter.StreamOnly = &stream
		case "retry":
			retry := value == "true"
			filter.RetryOnly = &retry
		case "timeout":
			filter.TimeoutOnly = value == "true"
		}
	}
	return filter, nil
}

func usageLogAgentRecord(row *database.UsageLog) logagent.Record {
	fields := map[string]string{
		"request_id":              row.RequestID,
		"upstream_request_id":     row.UpstreamRequestID,
		"parent_request_id":       row.ParentRequestID,
		"endpoint":                firstNonEmpty(row.InboundEndpoint, row.Endpoint),
		"upstream_endpoint":       row.UpstreamEndpoint,
		"model":                   row.Model,
		"effective_model":         row.EffectiveModel,
		"upstream_response_model": row.UpstreamResponseModel,
		"channel":                 row.Channel,
		"api_key":                 row.APIKeyName,
		"upstream_proxy":          row.UpstreamProxyName,
		"internal_reason":         row.InternalReason,
		"client_user_agent":       row.ClientUserAgent,
		"reasoning_effort":        row.ReasoningEffort,
		"service_tier":            row.ServiceTier,
		"account_email":           row.AccountEmail,
	}
	if row.AccountID > 0 {
		fields["account"] = strings.TrimSpace(fmt.Sprintf("#%d %s", row.AccountID, row.AccountName))
	}
	if row.AttemptIndex > 0 || row.IsRetryAttempt {
		fields["attempt"] = fmt.Sprintf("%d retry=%t", row.AttemptIndex, row.IsRetryAttempt)
	}
	if row.DurationMs > 0 {
		fields["duration_ms"] = strconv.Itoa(row.DurationMs)
	}
	if row.FirstTokenMs > 0 {
		fields["first_token_ms"] = strconv.Itoa(row.FirstTokenMs)
	}
	for name, enabled := range map[string]bool{"stream": row.Stream, "via_websocket": row.ViaWebsocket, "compact": row.Compact} {
		if enabled {
			fields[name] = "true"
		}
	}
	return logagent.Record{
		ID:        fmt.Sprintf("usage:%d", row.ID),
		Kind:      "usage_log",
		Time:      row.CreatedAt,
		Status:    row.StatusCode,
		ErrorKind: row.UpstreamErrorKind,
		Message:   row.ErrorMessage,
		Fields:    fields,
	}
}
