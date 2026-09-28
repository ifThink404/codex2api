package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/logagent"
	"github.com/gin-gonic/gin"
)

type fakeLogAgentLLM struct {
	output string
	err    error
	calls  int
	source string
	model  string
	input  string
}

func (f *fakeLogAgentLLM) RespondWithUsage(ctx context.Context, model, instructions, input string) (string, logagent.Usage, error) {
	f.calls++
	f.model, f.input = model, input
	return f.output, logagent.Usage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150}, f.err
}

func (f *fakeLogAgentLLM) Respond(ctx context.Context, model, instructions, input string) (string, error) {
	output, _, err := f.RespondWithUsage(ctx, model, instructions, input)
	return output, err
}

func newLogAgentTestHandler(t *testing.T) (*Handler, *gin.Engine, *database.DB, *fakeLogAgentLLM) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	llm := &fakeLogAgentLLM{output: `{"summary":"upstream overloaded","root_causes":[{"title":"503 from upstream","category":"upstream","evidence_ids":["usage:1","usage:404"],"confidence":0.8}],"suggested_actions":[{"title":"Pause account","priority":"high"}],"confidence":0.75}`}
	handler.logAgent.newLLM = func(h *Handler, key *database.APIKeyRow, source string, timeout time.Duration) logagent.LLM {
		llm.source = source
		return llm
	}
	router := gin.New()
	handler.registerLogAgentRoutes(router.Group("/api/admin"))
	return handler, router, db, llm
}

func doLogAgentRequest(t *testing.T, router *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	var payload map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	return recorder, payload
}

func seedLogAgentUsageLogs(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()
	accountID := insertTestAccount(t, db)
	for _, input := range []*database.UsageLogInput{
		{RequestID: "req-a", AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-5", StatusCode: http.StatusServiceUnavailable,
			UpstreamErrorKind: "server_overloaded", ErrorMessage: "upstream 503 Bearer sk-abcdefghijklmnopqrstuvwxyz0123"},
		{RequestID: "req-a", AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-5", StatusCode: http.StatusOK, AttemptIndex: 1, IsRetryAttempt: true},
		{RequestID: "req-b", AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-5", StatusCode: http.StatusTooManyRequests,
			UpstreamErrorKind: "rate_limited", ErrorMessage: "rate limited"},
		{RequestID: "req-c", AccountID: accountID, Endpoint: "/v1/chat/completions", Model: "gpt-5", StatusCode: http.StatusOK},
	} {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.FlushUsageLogs()
}

func TestLogAgentConfigRoundTrip(t *testing.T) {
	_, router, db, _ := newLogAgentTestHandler(t)
	recorder, payload := doLogAgentRequest(t, router, http.MethodGet, "/api/admin/log-agent/config", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	config := payload["config"].(map[string]any)
	if config["enabled"] != false || config["timeout_seconds"].(float64) != database.DefaultLogAgentTimeoutSeconds {
		t.Fatalf("default config = %v", config)
	}
	sources := payload["sources"].([]any)
	if !containsAny(sources, "usage_logs") || !containsAny(sources, "ops_errors") {
		t.Fatalf("sources = %v", sources)
	}

	recorder, _ = doLogAgentRequest(t, router, http.MethodPut, "/api/admin/log-agent/config", `{"enabled":true,"api_key_id":9999,"model":"gpt-5"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown key status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	keyID, err := db.InsertAPIKey(context.Background(), "ops", "sk-test-log-agent-key-0001")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"enabled":true,"api_key_id":` + logAgentJSONNumber(keyID) + `,"model":" gpt-5-mini ","max_input_bytes":1,"timeout_seconds":45,"retention_days":7}`
	recorder, payload = doLogAgentRequest(t, router, http.MethodPut, "/api/admin/log-agent/config", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	saved := payload["config"].(map[string]any)
	if saved["model"] != "gpt-5-mini" || saved["max_input_bytes"].(float64) != logagent.MinMaxInputBytes || saved["timeout_seconds"].(float64) != 45 {
		t.Fatalf("saved = %v", saved)
	}
	_, payload = doLogAgentRequest(t, router, http.MethodGet, "/api/admin/log-agent/config", "")
	keys := payload["gateway_keys"].([]any)
	if len(keys) != 1 || strings.Contains(recorder.Body.String(), "sk-test-log-agent-key-0001") {
		t.Fatalf("gateway keys = %v", keys)
	}
}

func TestLogAgentAnalyzeRequiresEnabledConfig(t *testing.T) {
	_, router, db, llm := newLogAgentTestHandler(t)
	recorder, _ := doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"ops_errors"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("disabled status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := db.SaveLogAgentConfig(context.Background(), database.LogAgentConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	recorder, _ = doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"ops_errors"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("no model status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := db.SaveLogAgentConfig(context.Background(), database.LogAgentConfig{Enabled: true, Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	recorder, _ = doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"nope"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown source status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, _ = doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"ops_errors","start":"2026-01-01T00:00:00Z"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("half range status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, _ = doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"ops_errors"}`)
	if recorder.Code != http.StatusUnprocessableEntity || llm.calls != 0 {
		t.Fatalf("empty status=%d calls=%d body=%s", recorder.Code, llm.calls, recorder.Body.String())
	}
}

func TestLogAgentAnalyzeOpsErrorsPersistsRun(t *testing.T) {
	_, router, db, llm := newLogAgentTestHandler(t)
	seedLogAgentUsageLogs(t, db)
	if _, err := db.SaveLogAgentConfig(context.Background(), database.LogAgentConfig{Enabled: true, Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"source":"ops_errors","filters":{"model":"gpt-5","status":"5xx"},"start":"` + start + `","end":"` + end + `","focus":"why 503?","language":"zh"}`
	recorder, payload := doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("analyze status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if llm.source != "ops_errors" || llm.model != "gpt-5" {
		t.Fatalf("llm source=%q model=%q", llm.source, llm.model)
	}
	if !strings.Contains(llm.input, "server_overloaded") || strings.Contains(llm.input, "rate limited") || strings.Contains(llm.input, "sk-abcdefghijklmnopqrstuvwxyz0123") {
		t.Fatalf("unexpected evidence:\n%s", llm.input)
	}
	run := payload["run"].(map[string]any)
	if run["status"] != database.LogAgentRunStatusSucceeded || run["total_tokens"].(float64) != 150 || run["record_count"].(float64) != 1 {
		t.Fatalf("run = %v", run)
	}
	findings := run["findings"].(map[string]any)
	causes := findings["root_causes"].([]any)
	evidence := causes[0].(map[string]any)["evidence_ids"].([]any)
	if findings["summary"] != "upstream overloaded" || len(evidence) != 1 {
		t.Fatalf("findings = %v", findings)
	}
	id := logAgentJSONNumber(int64(run["id"].(float64)))

	recorder, payload = doLogAgentRequest(t, router, http.MethodGet, "/api/admin/log-agent/runs/"+id, "")
	if recorder.Code != http.StatusOK || payload["run"].(map[string]any)["subject"].(map[string]any)["focus"] != "why 503?" {
		t.Fatalf("detail status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, payload = doLogAgentRequest(t, router, http.MethodGet, "/api/admin/log-agent/runs?source=ops_errors", "")
	if recorder.Code != http.StatusOK || len(payload["runs"].([]any)) != 1 {
		t.Fatalf("list status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder, _ = doLogAgentRequest(t, router, http.MethodGet, "/api/admin/log-agent/runs/999999", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing detail status=%d", recorder.Code)
	}
}

func TestLogAgentAnalyzeUsageLogRefsFallbackAndFailure(t *testing.T) {
	_, router, db, llm := newLogAgentTestHandler(t)
	seedLogAgentUsageLogs(t, db)
	if _, err := db.SaveLogAgentConfig(context.Background(), database.LogAgentConfig{Enabled: true, Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	llm.output = "not json at all"
	recorder, payload := doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"usage_logs","refs":["req-a","req-a",""]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("analyze status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	run := payload["run"].(map[string]any)
	if run["status"] != database.LogAgentRunStatusFallback || run["record_count"].(float64) != 2 {
		t.Fatalf("run = %v", run)
	}
	if !strings.Contains(llm.input, "req-a") || !strings.Contains(llm.input, "retry=true") {
		t.Fatalf("usage_logs refs should include every attempt:\n%s", llm.input)
	}

	llm.err = errors.New("HTTP 503")
	recorder, payload = doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"usage_logs","refs":["req-b"]}`)
	failed := payload["run"].(map[string]any)
	if recorder.Code != http.StatusBadGateway || failed["status"] != database.LogAgentRunStatusFailed || failed["error_message"] != "HTTP 503" || payload["error"] != "HTTP 503" {
		t.Fatalf("failure status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if findings := failed["findings"].(map[string]any); len(findings) != 0 {
		t.Fatalf("failed run must not carry empty findings: %v", findings)
	}
	runs, err := db.ListLogAgentRuns(context.Background(), database.LogAgentRunFilter{})
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %d err=%v", len(runs), err)
	}
}

func TestLogAgentAnalyzePluginSource(t *testing.T) {
	_, router, db, llm := newLogAgentTestHandler(t)
	if _, err := db.SaveLogAgentConfig(context.Background(), database.LogAgentConfig{Enabled: true, Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	var gotRefs []string
	err := logagent.Register(logagent.SourceFunc("test.plugin_captures", func(ctx context.Context, q logagent.Query) ([]logagent.Record, error) {
		gotRefs = q.Refs
		return []logagent.Record{{ID: "cap:1", Kind: "capture", Status: 502, Message: "plugin upstream reset", Body: "access_token=abc123secret"}}, nil
	}))
	if err != nil && !errors.Is(err, logagent.ErrDuplicateSource) {
		t.Fatal(err)
	}
	recorder, _ := doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"test.plugin_captures","refs":["cap:1"]}`)
	if recorder.Code != http.StatusOK || strings.Join(gotRefs, ",") != "cap:1" || llm.source != "test.plugin_captures" {
		t.Fatalf("status=%d refs=%v source=%q body=%s", recorder.Code, gotRefs, llm.source, recorder.Body.String())
	}
	if strings.Contains(llm.input, "abc123secret") {
		t.Fatalf("plugin body leaked secret:\n%s", llm.input)
	}
}

func TestLogAgentDefaultLLMUsesPoolWithInternalReason(t *testing.T) {
	handler := &Handler{}
	llm, ok := handler.newLogAgentLLM(nil, "ops_errors", time.Minute).(*poolLogAgentLLM)
	if !ok || llm.reason != "log_agent:ops_errors" || llm.timeout != time.Minute {
		t.Fatalf("default llm = %#v", llm)
	}
	if _, err := llm.Respond(context.Background(), "gpt-5", "i", "x"); !errors.Is(err, logagent.ErrNoLLM) {
		t.Fatalf("nil proxy err = %v", err)
	}
}

func TestLogAgentAnalyzeRejectsUnusableGatewayKey(t *testing.T) {
	_, router, db, llm := newLogAgentTestHandler(t)
	seedLogAgentUsageLogs(t, db)
	ctx := context.Background()
	keyID, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "expired", Key: "sk-test-log-agent-expired-01",
		ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveLogAgentConfig(ctx, database.LogAgentConfig{Enabled: true, Model: "gpt-5", APIKeyID: keyID}); err != nil {
		t.Fatal(err)
	}
	recorder, payload := doLogAgentRequest(t, router, http.MethodPost, "/api/admin/log-agent/analyze", `{"source":"usage_logs","refs":["req-a"]}`)
	if recorder.Code != http.StatusBadRequest || payload["error"] != "选择的网关 API Key 当前不可用" || llm.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, llm.calls, recorder.Body.String())
	}
}

func containsAny(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func logAgentJSONNumber(value int64) string {
	payload, _ := json.Marshal(value)
	return string(payload)
}
