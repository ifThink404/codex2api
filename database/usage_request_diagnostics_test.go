package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUsageRequestDiagnosticsPersistenceAndLightweightLists(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "diagnostics.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	payload := fmt.Sprintf(`{"version":1,"selected_account_id":17,"incoming":{"client_metadata":{"thread_source":"guardian_review","captured_value":%q}}}`, strings.Repeat("诊断值", 16*1024))
	if err := db.InsertUsageLog(test.Context(), &UsageLogInput{Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, RequestType: "related_internal", RequestDiagnostics: payload, UpstreamResponseModel: "gpt-5.6-luna", SessionIDPrefix: "01a09012", WindowNumberOriginal: "47", WindowNumberOutbound: "0", BPSAgentIteration: "25"}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Page: 1, PageSize: 10}
	lists := []struct {
		name string
		read func() ([]*UsageLog, error)
	}{
		{"recent", func() ([]*UsageLog, error) { return db.ListRecentUsageLogs(test.Context(), 10) }},
		{"time_range", func() ([]*UsageLog, error) {
			return db.ListUsageLogsByTimeRange(test.Context(), filter.Start, filter.End)
		}},
		{"filter", func() ([]*UsageLog, error) { return db.ListUsageLogsByFilter(test.Context(), filter) }},
		{"paged", func() ([]*UsageLog, error) {
			page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
			if err != nil {
				return nil, err
			}
			return page.Logs, nil
		}},
	}
	for _, item := range lists {
		test.Run(item.name, func(test *testing.T) {
			logs, err := item.read()
			if err != nil || len(logs) != 1 {
				test.Fatalf("logs=%+v, err=%v", logs, err)
			}
			if logs[0].RequestType != "related_internal" || logs[0].SessionIDPrefix != "01a09012" {
				test.Fatalf("missing type: %+v", logs[0])
			}
			if logs[0].UpstreamResponseModel != "gpt-5.6-luna" {
				test.Fatalf("missing response model: %+v", logs[0])
			}
			encoded, err := json.Marshal(logs)
			if logs[0].WindowNumberOriginal != "47" || logs[0].WindowNumberOutbound != "0" {
				test.Fatalf("window numbers missing from list: %+v", logs[0])
			}
			if logs[0].BPSAgentIteration != "25" {
				test.Fatalf("BPS iteration missing from list: %+v", logs[0])
			}
			if err != nil {
				test.Fatal(err)
			}
			if strings.Contains(string(encoded), "guardian_review") || strings.Contains(string(encoded), "request_diagnostics") {
				test.Fatal("normal list includes full diagnostics")
			}
			detail, err := db.GetUsageRequestDiagnostics(test.Context(), logs[0].ID)
			if err != nil || detail.RequestType != "related_internal" || string(detail.Diagnostics) != payload {
				test.Fatalf("detail=%+v, err=%v", detail, err)
			}
		})
	}
	if _, err := db.GetUsageRequestDiagnostics(test.Context(), 999999); !errors.Is(err, sql.ErrNoRows) {
		test.Fatalf("missing row error: %v", err)
	}
	// Both export paths must return the same complete snapshot as the detail API.
	snapshotID, err := db.UsageLogExportSnapshot(test.Context())
	if err != nil {
		test.Fatal(err)
	}
	for _, paged := range []bool{false, true} {
		count := 0
		visit := func(entry *UsageLogExportEntry) error {
			count++
			if string(entry.Diagnostics) != payload {
				test.Fatal("export lost diagnostic content")
			}
			return nil
		}
		if paged {
			err = db.WalkUsageLogExportPage(test.Context(), &filter, &UsageLogExportCursor{SnapshotID: snapshotID}, visit)
		} else {
			err = db.WalkUsageLogsForExport(test.Context(), &filter, visit)
		}
		if err != nil || count != 1 {
			test.Fatalf("paged=%t count=%d err=%v", paged, count, err)
		}
	}
}

func TestUsageRequestDiagnosticsSQLiteMigrationAndHistoricalRows(test *testing.T) {
	path := filepath.Join(test.TempDir(), "migration.db")
	db, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	_, err = db.conn.ExecContext(test.Context(), `INSERT INTO usage_logs (endpoint, model, status_code) VALUES ('/v1/responses', 'old', 200)`)
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN request_type`)
	}
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN request_diagnostics`)
	}
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN session_id_prefix`)
	}
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN window_number_original`)
	}
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN window_number_outbound`)
	}
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN upstream_response_model`)
	}
	_ = db.Close()
	if err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	logs, err := db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 1 || logs[0].RequestType != "" || logs[0].SessionIDPrefix != "" || logs[0].WindowNumberOriginal != "" || logs[0].WindowNumberOutbound != "" || logs[0].UpstreamResponseModel != "" {
		test.Fatalf("historical logs=%+v, err=%v", logs, err)
	}
	detail, err := db.GetUsageRequestDiagnostics(test.Context(), logs[0].ID)
	if err != nil || detail.Diagnostics != nil || detail.RequestType != "" {
		test.Fatalf("historical diagnostics must not be inferred: %+v, %v", detail, err)
	}
	if err := db.InsertUsageLog(test.Context(), &UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses"}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	logs, err = db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 2 || logs[0].RequestType != "unknown" {
		test.Fatalf("new unclassified row: %+v, %v", logs, err)
	}
	detail, err = db.GetUsageRequestDiagnostics(test.Context(), logs[0].ID)
	if err != nil || !strings.Contains(string(detail.Diagnostics), "not_available") {
		test.Fatalf("missing capture status: %+v, %v", detail, err)
	}
}

type usageDiagnosticSQLCapture struct {
	query string
	args  []interface{}
}

func (capture *usageDiagnosticSQLCapture) ExecContext(_ context.Context, query string, args ...interface{}) (sql.Result, error) {
	capture.query, capture.args = query, args
	return nil, nil
}

func TestUsageRequestDiagnosticsPostgresBatchShape(test *testing.T) {
	capture := &usageDiagnosticSQLCapture{}
	db := &DB{}
	length, decoded := 292, 217
	batch := []usageLogEntry{
		{RequestType: "user", RequestDiagnostics: `{"version":1}`, NewAPIUserName: "window-user", RequestID: "request-1", UpstreamRequestID: "upstream-1", UpstreamProxyID: 12, UpstreamProxyName: "proxy-1", ImageInputTokens: 7, ImageOutputTokens: 11, CachedImageInputTokens: 3, SessionIDPrefix: "01a09012"},
		{RequestType: "compaction", RequestDiagnostics: fmt.Sprintf(`{"version":1,"attempt":2,"incoming":{"headers":{"User-Agent":%q}}}`, strings.Repeat("x", 64*1024)), RequestID: "request-2", UpstreamRequestID: "upstream-2", WindowNumberOriginal: "18446744073709551615", WindowNumberOutbound: "0"},
	}
	batch[0].TurnStateLength, batch[0].TurnStateDecodedBytes = &length, &decoded
	first := true
	batch[0].TurnID, batch[0].IsTurnFirstRequest, batch[0].TurnPromptPreview = "turn-a", &first, "请检查接口"
	batch[0].BPSAgentIteration = "25"
	if err := db.batchInsertLogsChunk(test.Context(), capture, batch); err != nil {
		test.Fatal(err)
	}
	if len(capture.args) != len(batch)*usageLogInsertColumnCount || !strings.Contains(capture.query, "request_type, request_diagnostics,") || !strings.Contains(capture.query, fmt.Sprintf("$%d)", len(capture.args))) {
		test.Fatalf("invalid batch shape: args=%d, query=%s", len(capture.args), capture.query)
	}
	columns := strings.Split(capture.query[strings.Index(capture.query, "(")+1:strings.Index(capture.query, ")")], ",")
	for index := range columns {
		columns[index] = strings.TrimSpace(columns[index])
	}
	if len(columns) != usageLogInsertColumnCount {
		test.Fatalf("columns=%d, expected %d", len(columns), usageLogInsertColumnCount)
	}
	for index, entry := range batch {
		for name, expected := range map[string]interface{}{
			"bps_agent_iteration": entry.BPSAgentIteration,
			"turn_id":             entry.TurnID, "is_turn_first_request": entry.IsTurnFirstRequest, "turn_prompt_preview": entry.TurnPromptPreview,
			"turn_state_length": entry.TurnStateLength, "turn_state_decoded_bytes": entry.TurnStateDecodedBytes,
			"session_id_prefix":      entry.SessionIDPrefix,
			"window_number_original": entry.WindowNumberOriginal, "window_number_outbound": entry.WindowNumberOutbound,
			"request_type": entry.RequestType, "request_diagnostics": entry.RequestDiagnostics, "newapi_user_name": entry.NewAPIUserName,
			"request_id": entry.RequestID, "upstream_request_id": entry.UpstreamRequestID, "upstream_proxy_id": entry.UpstreamProxyID, "upstream_proxy_name": entry.UpstreamProxyName,
			"image_input_tokens": entry.ImageInputTokens, "image_output_tokens": entry.ImageOutputTokens, "cached_image_input_tokens": entry.CachedImageInputTokens,
		} {
			columnIndex := slices.Index(columns, name)
			if columnIndex < 0 || capture.args[index*usageLogInsertColumnCount+columnIndex] != expected {
				test.Fatalf("row %d: column %s was not preserved", index, name)
			}
		}
	}
}

func TestUsageRequestDiagnosticsPreservationAndLoggingModes(test *testing.T) {
	for _, size := range []int{12*1024 - 1, 12 * 1024, 12*1024 + 1, 256 * 1024, 1024 * 1024} {
		payload := fmt.Sprintf(`{"version":1,"incoming":{"headers":{"value":%q}}}`, strings.Repeat("x", size))
		if normalizeUsageRequestDiagnostics(payload) != payload {
			test.Fatalf("normalization discarded %d-byte snapshot", len(payload))
		}
	}
	if !json.Valid([]byte(normalizeUsageRequestDiagnostics(""))) {
		test.Fatal("invalid missing-capture handling")
	}
	db, err := New("sqlite", filepath.Join(test.TempDir(), "mode.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	for _, mode := range []string{UsageLogModeOff, UsageLogModeErrors} {
		db.SetUsageLogConfig(mode, 100, 10)
		if err := db.InsertUsageLog(test.Context(), &UsageLogInput{StatusCode: 200, RequestType: "user", RequestDiagnostics: `{"version":1}`}); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 0 {
		test.Fatalf("diagnostics changed log mode: %+v, %v", logs, err)
	}
}
