package admin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/logagent"
	"github.com/codex2api/proxy/plugins"
)

// Each transport plugin's capture store is a log-agent source named
// "<plugin>.captures" (e.g. bps.captures). Refs are capture IDs ("cap:<id>"
// or bare numbers) or request IDs; filters use the capture page's names
// (request_id, account_id, status, direction). Record IDs are "cap:<id>";
// the masked body travels in Body, which the agent trims first.

const logAgentCaptureHeadersLimit = 4096

func registerPluginCaptureLogAgentSources(registry *logagent.Registry, db *database.DB) {
	for _, p := range plugins.Default().Plugins() {
		plugin := p.ID()
		_ = registry.Register(logagent.SourceFunc(plugin+".captures", func(ctx context.Context, q logagent.Query) ([]logagent.Record, error) {
			return fetchPluginCaptureRecords(ctx, db, plugin, q)
		}))
	}
}

func fetchPluginCaptureRecords(ctx context.Context, db *database.DB, plugin string, q logagent.Query) ([]logagent.Record, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = logagent.DefaultMaxRecords
	}
	var captures []*database.PluginCapture
	if len(q.Refs) > 0 {
		for _, ref := range q.Refs {
			ref = strings.TrimSpace(ref)
			if id, err := strconv.ParseInt(strings.TrimPrefix(ref, "cap:"), 10, 64); err == nil && id > 0 {
				capture, err := db.GetPluginCapture(ctx, id)
				if err != nil {
					return nil, err
				}
				if capture != nil && capture.Plugin == plugin {
					captures = append(captures, capture)
				}
			} else if ref != "" {
				rows, err := listPluginCapturesWithBodies(ctx, db, database.PluginCaptureFilter{Plugin: plugin, RequestID: ref, PageSize: limit})
				if err != nil {
					return nil, err
				}
				captures = append(captures, rows...)
			}
			if len(captures) >= limit {
				captures = captures[:limit]
				break
			}
		}
	} else {
		filter, err := pluginCaptureFilterFromLogAgent(plugin, q)
		if err != nil {
			return nil, err
		}
		filter.PageSize = min(limit, 200)
		if captures, err = listPluginCapturesWithBodies(ctx, db, filter); err != nil {
			return nil, err
		}
	}
	records := make([]logagent.Record, 0, len(captures))
	for _, capture := range captures {
		records = append(records, pluginCaptureAgentRecord(capture))
	}
	return records, nil
}

// List rows omit bodies; the agent needs them, so read each full row.
func listPluginCapturesWithBodies(ctx context.Context, db *database.DB, filter database.PluginCaptureFilter) ([]*database.PluginCapture, error) {
	page, err := db.ListPluginCaptures(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]*database.PluginCapture, 0, len(page.Captures))
	for _, row := range page.Captures {
		full, err := db.GetPluginCapture(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		if full != nil {
			out = append(out, full)
		}
	}
	return out, nil
}

func pluginCaptureFilterFromLogAgent(plugin string, q logagent.Query) (database.PluginCaptureFilter, error) {
	filter := database.PluginCaptureFilter{Plugin: plugin, Start: q.Start, End: q.End}
	if filter.Start.IsZero() && filter.End.IsZero() {
		filter.Start = time.Now().Add(-logAgentDefaultFilterWindow)
	}
	for key, value := range q.Filters {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch key {
		case "request_id":
			filter.RequestID = value
		case "direction":
			filter.Direction = value
		case "account_id":
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				return filter, fmt.Errorf("account_id 参数无效")
			}
			filter.AccountID = &id
		case "status":
			status, err := strconv.Atoi(value)
			if err != nil || status < 0 {
				return filter, fmt.Errorf("status 参数无效")
			}
			filter.Status = &status
		}
	}
	return filter, nil
}

func pluginCaptureAgentRecord(capture *database.PluginCapture) logagent.Record {
	headers := capture.Headers
	if len(headers) > logAgentCaptureHeadersLimit {
		headers = headers[:logAgentCaptureHeadersLimit]
	}
	fields := map[string]string{
		"request_id": capture.RequestID,
		"plugin":     capture.Plugin,
		"direction":  capture.Direction,
		"attempt":    strconv.Itoa(capture.Attempt),
		"headers":    headers,
	}
	if capture.AccountID > 0 {
		fields["account"] = fmt.Sprintf("#%d", capture.AccountID)
	}
	if capture.Truncated {
		fields["truncated"] = "true"
	}
	return logagent.Record{
		ID:        fmt.Sprintf("cap:%d", capture.ID),
		Kind:      "plugin_capture",
		Time:      capture.CreatedAt,
		Status:    capture.Status,
		ErrorKind: capture.ErrorKind,
		Message:   capture.Direction,
		Fields:    fields,
		Body:      capture.Body,
	}
}
