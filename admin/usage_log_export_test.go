package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestUsageLogExportConfirmsScopeAndRedactsSecrets(t *testing.T) {
	db := newTestAdminDB(t)
	accountID, err := db.InsertAccountWithCredentials(t.Context(), "account-name", map[string]interface{}{
		"refresh_token": "never-export-this", "email": "export@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	for _, model := range []string{"model-a", "model-b"} {
		if err := db.InsertUsageLog(t.Context(), &database.UsageLogInput{
			AccountID: accountID, StatusCode: 500, ErrorMessage: "server_is_overloaded · Bearer secret-text", Endpoint: "/v1/responses",
			Model: model, RequestID: "request-" + model, ClientUserAgent: "Codex Desktop/0.150.0",
		}); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.FlushUsageLogs()
	handler := &Handler{db: db}
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	params := url.Values{"confirmed": {"true"}, "start": {time.Now().Add(-time.Hour).Format(time.RFC3339)}, "end": {time.Now().Add(time.Hour).Format(time.RFC3339)}, "model": {"model-b"}, "q": {"api_key=sk-leak123"}, "page": {"99"}, "page_size": {"1"}}
	for _, scope := range []string{"filtered", "all"} {
		params.Set("scope", scope)
		response := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(response)
		request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+params.Encode(), nil)
		if scope == "filtered" {
			params.Del("q")
			request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+params.Encode(), nil)
		}
		handler.ExportUsageLogs(request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s export status = %d: %s", scope, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Header().Get("Content-Disposition"), "usage-logs-"+scope) || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s export headers = %v", scope, response.Header())
		}
		var result struct {
			Scope    string            `json:"scope"`
			Total    int               `json:"total"`
			Complete bool              `json:"complete"`
			Filters  map[string]string `json:"filters"`
			Logs     []json.RawMessage `json:"logs"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode %s export: %v\n%s", scope, err, response.Body.String())
		}
		want := 1
		if scope == "all" {
			want = 2
			if len(result.Filters) != 0 {
				t.Fatalf("all export must not echo filters: %v", result.Filters)
			}
		}
		if result.Scope != scope || !result.Complete || result.Total != want || len(result.Logs) != want {
			t.Fatalf("%s export = scope %q complete %v total %d logs %d, want %d", scope, result.Scope, result.Complete, result.Total, len(result.Logs), want)
		}
		body := response.Body.String()
		for _, forbidden := range []string{"never-export-this", "secret-text", "sk-leak123"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s export leaked %q", scope, forbidden)
			}
		}
		if !strings.Contains(body, "export@example.com") || !strings.Contains(body, "Codex Desktop/0.150.0") {
			t.Fatalf("%s export dropped list-visible fields", scope)
		}
		if files, err := os.ReadDir(temp); err != nil || len(files) != 0 {
			t.Fatalf("temporary export file left behind: %v %v", files, err)
		}
	}
}

func TestUsageLogExportRejectsMissingConfirmationAndInvalidFilters(t *testing.T) {
	handler := &Handler{}
	for _, query := range []string{"scope=all", "scope=all&confirmed=false", "scope=invalid&confirmed=true", "scope=filtered&confirmed=true", "scope=filtered&confirmed=true&start=2026-09-12T00:00:00Z&end=2026-09-11T00:00:00Z", "scope=filtered&confirmed=true&start=2026-09-11T00:00:00Z&end=2026-09-12T00:00:00Z&channel=invalid"} {
		response := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(response)
		request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+query, nil)
		handler.ExportUsageLogs(request)
		if response.Code != http.StatusBadRequest || response.Header().Get("Content-Disposition") != "" {
			t.Fatalf("%s: status %d disposition %q", query, response.Code, response.Header().Get("Content-Disposition"))
		}
	}
	handler.usageLogExportBusy.Store(true)
	response := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(response)
	request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?scope=all&confirmed=true", nil)
	handler.ExportUsageLogs(request)
	if response.Code != http.StatusConflict {
		t.Fatalf("concurrent export status = %d, want 409", response.Code)
	}
}

func TestUsageLogExportRowCapAndCancellation(t *testing.T) {
	db := newTestAdminDB(t)
	for index := 0; index < 3; index++ {
		if err := db.InsertUsageLog(t.Context(), &database.UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses"}); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.FlushUsageLogs()
	handler := &Handler{db: db}
	all := database.UsageLogFilter{Start: time.Unix(0, 0).UTC(), End: time.Now().Add(time.Hour), IncludeCanceled: true}
	var output bytes.Buffer
	if err := handler.writeUsageLogExport(t.Context(), &output, "all", map[string]string{}, all, time.Now(), 2); err != nil {
		t.Fatalf("writeUsageLogExport: %v", err)
	}
	var result struct {
		Logs     []any `json:"logs"`
		Total    int   `json:"total"`
		Complete bool  `json:"complete"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, output.String())
	}
	if len(result.Logs) != 2 || result.Total != 2 || result.Complete {
		t.Fatalf("capped export = logs %d total %d complete %v", len(result.Logs), result.Total, result.Complete)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(response)
	request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?scope=all&confirmed=true", nil).WithContext(ctx)
	handler.ExportUsageLogs(request)
	if response.Code < http.StatusBadRequest || response.Header().Get("Content-Disposition") != "" || strings.Contains(response.Body.String(), `"complete":true`) {
		t.Fatalf("canceled export returned a download: %d %s", response.Code, response.Body.String())
	}
	if handler.usageLogExportBusy.Load() {
		t.Fatal("export lock not released")
	}
}
