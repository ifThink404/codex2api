package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func newServiceErrorTestHandler(test *testing.T) *Handler {
	test.Helper()
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "service-errors.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(db, nil, nil)
	test.Cleanup(store.Stop)
	return NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
}

func serviceErrorTestPage(test *testing.T, handler *Handler) database.ServiceErrorPage {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for handler.db.ServiceErrorCollectorStats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stats := handler.db.ServiceErrorCollectorStats(); stats.Pending != 0 || stats.WriteFailures != 0 {
		test.Fatalf("collector unhealthy: %+v", stats)
	}
	page, err := handler.db.ListServiceErrors(context.Background(), database.ServiceErrorFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Second), Limit: 100})
	if err != nil {
		test.Fatal(err)
	}
	return page
}

func TestServiceErrorsGlobalRateLimitBeforeAuthentication(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware(), NewRateLimiter(1).Middleware())
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) { ctx.Status(http.StatusOK) })
	var rejected int
	for range 6 {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if recorder.Code == http.StatusTooManyRequests {
			rejected++
			if recorder.Header().Get("X-Codex2API-Request-ID") == "" {
				test.Fatal("missing correlation header")
			}
		}
	}
	page := serviceErrorTestPage(test, handler)
	if rejected == 0 || page.Summary.Total != int64(rejected) {
		test.Fatalf("missing global rejections: rejected=%d page=%+v", rejected, page)
	}
	for _, event := range page.Items {
		if event.StatusCode != 429 || event.Stage != "rate_limit" || event.Code != "rate_limit_exceeded" || event.APIKeyID != 0 {
			test.Fatalf("incorrect global error: %+v", event)
		}
	}
}

func TestServiceErrorsHTTPDedupeRedactionAndUpstreamExclusion(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/local", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		ctx.Set("apiKey", "local-secret-that-must-not-leak")
		ctx.Set("x-model", "gpt-6-astra")
		api.SendErrorWithStatus(ctx, api.NewAPIError(api.ErrCodeRateLimitReached, "limit local-secret-that-must-not-leak", api.ErrorTypeRateLimit), 429)
		// A second error in the same request must not add another row.
		api.ObserveError(ctx, 500, api.NewAPIError(api.ErrCodeServerError, "second", api.ErrorTypeServer))
	})
	router.POST("/v1/upstream", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		handler.sendUpstreamError(ctx, 429, []byte(`{"error":{"code":"usage_limit_reached","message":"upstream quota exhausted"}}`))
	})
	router.GET("/api/admin/unrelated", func(ctx *gin.Context) { api.SendError(ctx, api.ErrInvalidAPIKey) })
	for _, path := range []string{"/v1/local", "/v1/upstream", "/api/admin/unrelated"} {
		method := http.MethodPost
		if strings.Contains(path, "/admin/") {
			method = http.MethodGet
		}
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path+"?token=query-secret", nil))
	}
	page := serviceErrorTestPage(test, handler)
	if page.Summary.Total != 1 || len(page.Items) != 1 {
		test.Fatalf("duplicated or included upstream/admin errors: %+v", page)
	}
	event := page.Items[0]
	payload, _ := json.Marshal(event)
	if strings.Contains(string(payload), "local-secret") || strings.Contains(string(payload), "query-secret") || event.Model != "gpt-6-astra" || event.RequestID == "" || event.Stage != "rate_limit" || event.Endpoint != "/v1/local" {
		test.Fatalf("unsafe service error: %s", payload)
	}
}

func TestServiceErrorsSkipErrorsAfterUpstreamAttemptExceptDispatch(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	attempt := func(ctx *gin.Context) {
		account := &auth.Account{DBID: 7}
		beginUpstreamTrace(ctx.Request.Context(), account, "", false)
	}
	router.POST("/v1/upstream-rendered", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		attempt(ctx)
		api.SendErrorWithStatus(ctx, api.NewAPIError(api.ErrCodeInvalidRequest, "upstream rejected the prompt", api.ErrorTypeInvalidRequest), 400)
	})
	router.POST("/v1/pool-exhausted", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		attempt(ctx)
		api.SendError(ctx, api.ErrServiceUnavailable)
	})
	for _, path := range []string{"/v1/upstream-rendered", "/v1/pool-exhausted"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
	}
	page := serviceErrorTestPage(test, handler)
	if len(page.Items) != 1 || page.Items[0].Endpoint != "/v1/pool-exhausted" || page.Items[0].Stage != "dispatch" {
		test.Fatalf("expected only the local dispatch failure: %+v", page.Items)
	}
}

func TestServiceErrorsPreBodyValidationAndPanic(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(api.RecoveryMiddleware(), handler.ServiceErrorMiddleware(), security.RequestSizeLimiter(16))
	router.POST("/v1/responses", func(ctx *gin.Context) { ctx.Status(200) })
	router.GET("/v1/panic", func(ctx *gin.Context) { panic("secret panic details") })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("secret-body", 100))))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		test.Fatalf("body rejection changed: %d", recorder.Code)
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/panic", nil))
	page := serviceErrorTestPage(test, handler)
	if page.Summary.Total != 2 || page.Summary.Status5xx != 1 {
		test.Fatalf("missing early service error: %+v", page)
	}
	for _, event := range page.Items {
		if strings.Contains(event.Message, "secret") {
			test.Fatalf("request or panic body persisted: %+v", event)
		}
	}
}

func TestServiceErrorsWebSocketFramesBeforeClose(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.GET("/v1/responses", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		connection, err := responsesWSUpgrader.Upgrade(ctx.Writer, ctx.Request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
			resetServiceErrorFrame(ctx)
			resetUpstreamRequestTrace(ctx)
			_ = writeAuditedResponsesWSError(ctx, connection, api.NewAPIError(api.ErrCodeRateLimitReached, "background concurrency full", api.ErrorTypeRateLimit))
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	header := http.Header{}
	header.Set("X-Codex-Turn-Metadata", `{"thread_source":"subagent","request_kind":"turn"}`)
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", header)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	if response.StatusCode != 101 {
		test.Fatalf("upgrade failed: %d", response.StatusCode)
	}
	for range 2 {
		if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":"private prompt"}`)); err != nil {
			test.Fatal(err)
		}
		if _, _, err := connection.ReadMessage(); err != nil {
			test.Fatal(err)
		}
	}
	page := serviceErrorTestPage(test, handler)
	if page.Summary.Total != 2 || page.Items[0].RequestID == page.Items[1].RequestID {
		test.Fatalf("per-turn errors not persisted before WS close: %+v", page)
	}
	for _, event := range page.Items {
		if event.Transport != "websocket" || event.StatusCode != 429 || event.ThreadSource != "subagent" || event.RequestKind != "turn" {
			test.Fatalf("missing WS context: %+v", event)
		}
	}
}

func TestServiceErrorWriterPreservesSuccessfulStream(test *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writer := &serviceErrorResponseWriter{ResponseWriter: ctx.Writer}
	body := strings.Repeat("private successful output", 10000)
	_, _ = io.WriteString(writer, body)
	writer.Flush()
	if recorder.Body.String() != body || len(writer.body) != 0 {
		test.Fatal("successful stream was buffered or changed")
	}
	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	writer = &serviceErrorResponseWriter{ResponseWriter: ctx.Writer}
	writer.WriteHeader(429)
	_, _ = io.WriteString(writer, body)
	if len(writer.body) != 8192 || recorder.Body.String() != body {
		test.Fatal("error buffer unbounded or client body changed")
	}
}

func TestServiceErrorUpstreamClassification(test *testing.T) {
	for _, code := range []api.ErrorCode{"upstream_429", "account_pool_unauthorized", "slow_down", "server_is_overloaded", "usage_limit_reached"} {
		if !serviceErrorIsUpstream(api.NewAPIError(code, "upstream", api.ErrorTypeServer)) {
			test.Errorf("upstream code classified local: %s", code)
		}
	}
	if serviceErrorIsUpstream(api.NewAPIError(api.ErrCodeInvalidRequest, "local validation", api.ErrorTypeInvalidRequest)) {
		test.Fatal("local validation classified as upstream")
	}
}

func TestServiceErrorsKeyQuotaKeepsCallerBeforeAuthenticationCompletes(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	keyID, err := handler.db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: "sk-quota-secret-never-log", Name: "quota caller", QuotaLimit: 1, QuotaUsed: 1,
	})
	if err != nil {
		test.Fatal(err)
	}
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) { ctx.Status(200) })
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"private"}`))
	request.Header.Set("Authorization", "Bearer sk-quota-secret-never-log")
	request.Header.Set("X-NewAPI-Request-ID", "2026091007254567890123unsigned")
	request.Header.Set("User-Agent", "codex_cli_rs/0.150.0 (Mac OS 26.0.0; arm64)")
	request.Header.Set("X-Codex-Turn-Metadata", `{"thread_source":"user","request_kind":"turn"}`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 429 {
		test.Fatalf("quota response changed: %d %s", recorder.Code, recorder.Body.String())
	}
	page := serviceErrorTestPage(test, handler)
	if len(page.Items) != 1 {
		test.Fatalf("missing quota error: %+v", page)
	}
	event := page.Items[0]
	if event.APIKeyID != keyID || event.APIKeyName != "quota caller" || event.NewAPIIdentityVerified || event.NewAPIRequestID != "2026091007254567890123unsigned" || event.ThreadSource != "user" || event.Stage != "rate_limit" {
		test.Fatalf("incorrect pre-authentication identity: %+v", event)
	}
	if event.RequestID != recorder.Header().Get("X-Codex2API-Request-ID") {
		test.Fatal("request correlation header does not match log")
	}
	if !strings.HasPrefix(event.ClientInfo["user_agent"], "codex_cli_rs/0.150.0") {
		test.Fatalf("missing client info: %+v", event.ClientInfo)
	}
	if payload, _ := json.Marshal(event); strings.Contains(string(payload), "quota-secret") {
		test.Fatalf("API key persisted: %s", payload)
	}
}

func TestResponsesInvalidToolsTypeIsSavedWithoutContents(test *testing.T) {
	for _, tc := range []struct{ name, value, kind string }{
		{"object", `{"private-key":{"type":"function","name":"private-tool"}}`, "object"},
		{"string", `"[{\"type\":\"function\",\"name\":\"private-tool\"}]"`, "string"},
		{"true", `true`, "boolean"},
		{"number", `1`, "number"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			handler := newServiceErrorTestHandler(test)
			router := gin.New()
			router.Use(handler.ServiceErrorMiddleware(), api.BodyCacheMiddleware())
			router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), handler.Responses)
			body := `{"model":"gpt-5.5","input":"private-prompt","tools":` + tc.value + `}`
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Field 'tools' must be an array") {
				test.Fatalf("validation response changed: %d %s", recorder.Code, recorder.Body.String())
			}
			page := serviceErrorTestPage(test, handler)
			if len(page.Items) != 1 {
				test.Fatalf("missing validation error: %+v", page)
			}
			event := page.Items[0]
			var shape map[string]string
			if err := json.Unmarshal(event.ToolProtocol, &shape); err != nil || event.Stage != "validation" || shape["top_level_tools_type"] != tc.kind {
				test.Fatalf("tools shape = %s stage=%s err=%v", event.ToolProtocol, event.Stage, err)
			}
			if encoded, _ := json.Marshal(event); strings.Contains(string(encoded), "private-") {
				test.Fatalf("request contents persisted: %s", encoded)
			}
		})
	}
	for _, body := range []string{`{"model":"gpt-5.5","input":"hello"}`, `{"model":"gpt-5.5","input":"hello","tools":[]}`} {
		if shape := serviceErrorToolsShape([]byte(body)); shape != nil {
			test.Fatalf("ordinary tools must not add diagnostics: %s", shape)
		}
	}
}
