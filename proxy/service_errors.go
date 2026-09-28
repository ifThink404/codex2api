package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const serviceErrorContextKey = "service_error_audit"

// serviceErrorAudit is the per-logical-request state of the service error
// collector. A Responses/Realtime WebSocket gets a fresh state per frame.
type serviceErrorAudit struct {
	started       time.Time
	recorded      atomic.Bool
	authenticated bool
	websocket     bool
	apiKeyID      int64
	apiKeyName    string
}

// serviceErrorResponseWriter keeps at most 8 KiB of an error response written
// without api.SendError (legacy c.JSON paths). Successful bodies are not copied.
type serviceErrorResponseWriter struct {
	gin.ResponseWriter
	mu   sync.Mutex
	body []byte
}

func (writer *serviceErrorResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *serviceErrorResponseWriter) capture(payload []byte) {
	if writer.Status() < 400 {
		return
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	remaining := 8192 - len(writer.body)
	if remaining > 0 {
		writer.body = append(writer.body, payload[:min(remaining, len(payload))]...)
	}
}

func (writer *serviceErrorResponseWriter) Write(payload []byte) (int, error) {
	written, err := writer.ResponseWriter.Write(payload)
	writer.capture(payload[:written])
	return written, err
}

func (writer *serviceErrorResponseWriter) WriteString(payload string) (int, error) {
	written, err := writer.ResponseWriter.WriteString(payload)
	if writer.Status() >= 400 {
		writer.capture([]byte(payload[:min(written, 8192)]))
	}
	return written, err
}

func serviceErrorAuditForRequest(ctx *gin.Context) *serviceErrorAudit {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Get(serviceErrorContextKey)
	state, _ := value.(*serviceErrorAudit)
	return state
}

// noteServiceErrorAPIKey records the resolved API key, even if it is then
// rejected as disabled or expired.
func noteServiceErrorAPIKey(ctx *gin.Context, apiKeyID int64, apiKeyName string) {
	if state := serviceErrorAuditForRequest(ctx); state != nil {
		state.apiKeyID, state.apiKeyName = apiKeyID, apiKeyName
	}
}

// markServiceErrorAuthenticated separates later 401/403 policy rejections from
// authentication failures.
func markServiceErrorAuthenticated(ctx *gin.Context) {
	if state := serviceErrorAuditForRequest(ctx); state != nil {
		state.authenticated = true
	}
}

// writeAuditedResponsesWSError reports a WebSocket error frame to the service
// error collector before sending it; upstream errors are filtered there.
func writeAuditedResponsesWSError(ctx *gin.Context, conn *websocket.Conn, apiError *api.APIError) error {
	if apiError != nil {
		api.ObserveError(ctx, api.HTTPStatusCode(apiError.Code), apiError)
	}
	return writeResponsesWSError(conn, apiError)
}

// resetServiceErrorFrame starts a new logical request on a WebSocket so each
// frame's local error is recorded once, keeping the connection's identity.
func resetServiceErrorFrame(ctx *gin.Context) {
	if previous := serviceErrorAuditForRequest(ctx); previous != nil {
		ctx.Set(serviceErrorContextKey, &serviceErrorAudit{
			started: time.Now(), authenticated: previous.authenticated, websocket: true,
			apiKeyID: previous.apiKeyID, apiKeyName: previous.apiKeyName,
		})
	}
	ctx.Set("x-model", "")
}

func (handler *Handler) beginServiceErrorAudit(ctx *gin.Context) func() {
	if handler == nil || handler.db == nil || serviceErrorAuditForRequest(ctx) != nil {
		return func() {}
	}
	ctx.Set(serviceErrorContextKey, &serviceErrorAudit{started: time.Now()})
	api.SetErrorObserver(ctx, handler.recordServiceError)
	writer := &serviceErrorResponseWriter{ResponseWriter: ctx.Writer}
	ctx.Writer = writer
	return func() {
		state := serviceErrorAuditForRequest(ctx)
		if state == nil || state.recorded.Load() || serviceErrorWebSocket(ctx, state) || writer.Status() < 400 || writer.Status() > 599 {
			return
		}
		// A response that followed an upstream attempt belongs to the usage/error logs.
		if snapshotUpstreamTrace(ctx.Request.Context()).accountID > 0 {
			return
		}
		writer.mu.Lock()
		body := append([]byte(nil), writer.body...)
		writer.mu.Unlock()
		handler.recordServiceError(ctx, writer.Status(), serviceErrorFromResponse(writer.Status(), body))
	}
}

// serviceErrorFromResponse rebuilds an API error from a legacy error body.
func serviceErrorFromResponse(status int, body []byte) *api.APIError {
	message, code, errorType := "Service request rejected", fmt.Sprintf("http_%d", status), string(api.ErrorTypeServer)
	if status < 500 {
		errorType = string(api.ErrorTypeInvalidRequest)
	}
	switch status {
	case http.StatusUnauthorized:
		errorType = string(api.ErrorTypeAuthentication)
	case http.StatusForbidden:
		errorType = string(api.ErrorTypePermission)
	case http.StatusNotFound:
		errorType = string(api.ErrorTypeNotFound)
	case http.StatusTooManyRequests:
		errorType = string(api.ErrorTypeRateLimit)
	}
	if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		for _, fields := range []gjson.Result{parsed, parsed.Get("error")} {
			if value := fields.Get("message"); value.Type == gjson.String && value.String() != "" {
				message = value.String()
			}
			if value := fields.Get("code"); value.Type == gjson.String && value.String() != "" {
				code = value.String()
			}
			if value := fields.Get("type"); value.Type == gjson.String && value.String() != "" {
				errorType = value.String()
			}
		}
		if failure := parsed.Get("error"); failure.Type == gjson.String && failure.String() != "" {
			message = failure.String()
		}
	}
	return api.NewAPIError(api.ErrorCode(code), message, api.ErrorType(errorType))
}

// ServiceErrorMiddleware collects gateway-local rejections for the proxy
// routes. It never changes the response status, body or retry behavior.
func (handler *Handler) ServiceErrorMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if !serviceErrorCollectedPath(ctx.Request.URL.Path) {
			ctx.Next()
			return
		}
		attachUpstreamTrace(ctx, handler.store)
		finish := handler.beginServiceErrorAudit(ctx)
		defer func() {
			if panicValue := recover(); panicValue != nil {
				api.ObserveError(ctx, http.StatusInternalServerError, api.NewAPIError(api.ErrCodeServerError, "Service handler panic", api.ErrorTypeServer))
				panic(panicValue)
			}
			finish()
		}()
		ctx.Next()
	}
}

func serviceErrorCollectedPath(path string) bool {
	if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1beta/") || strings.HasPrefix(path, "/backend-api/codex/") {
		return true
	}
	for _, prefix := range []string{"/responses", "/chat/completions", "/messages", "/images", "/videos", "/realtime", "/models", "/alpha/search", "/live"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// serviceErrorWebSocket is true for upgraded connections, including errors
// written before the first frame reset.
func serviceErrorWebSocket(ctx *gin.Context, state *serviceErrorAudit) bool {
	return state.websocket || (ctx.Request != nil && strings.EqualFold(strings.TrimSpace(ctx.Request.Header.Get("Upgrade")), "websocket"))
}

func serviceErrorIsUpstream(apiError *api.APIError) bool {
	code := strings.ToLower(string(apiError.Code))
	return apiError.Type == api.ErrorTypeUpstream || strings.HasPrefix(code, "upstream_") ||
		strings.HasPrefix(code, "account_pool_") || code == "server_is_overloaded" || code == "slow_down" ||
		code == "usage_limit_reached" || code == "insufficient_quota"
}

func serviceErrorStage(state *serviceErrorAudit, status int, code string) string {
	code = strings.ToLower(code)
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case !state.authenticated && (status == http.StatusUnauthorized || status == http.StatusForbidden || code == "service_unavailable"):
		return "authentication"
	case strings.Contains(code, "policy") || strings.Contains(code, "prompt") || strings.Contains(code, "newapi") || strings.Contains(code, "cyber"):
		return "policy"
	case code == "service_unavailable" || code == "no_available_account" || strings.HasPrefix(code, "codex_dispatch"):
		return "dispatch"
	case status < 500:
		return "validation"
	default:
		return "internal"
	}
}

func serviceErrorSafeText(ctx *gin.Context, value string, limit int) string {
	if value == "" {
		return ""
	}
	value = security.SafeTruncate(value, 8192)
	if secret := ctx.GetString("apiKey"); secret != "" {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	if ctx.Request != nil {
		if secret := strings.TrimSpace(strings.TrimPrefix(downstreamAuthorizationHeader(ctx.Request), "Bearer ")); secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return strings.Clone(security.SafeTruncate(security.MaskSensitiveData(value), limit))
}

// serviceErrorLabel keeps short protocol labels (codes, models, sources) and
// drops anything that could carry free text.
func serviceErrorLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 160 {
		return ""
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case strings.ContainsRune("._:/@+-", char):
		default:
			return ""
		}
	}
	return value
}

func (handler *Handler) recordServiceError(ctx *gin.Context, status int, apiError *api.APIError) {
	state := serviceErrorAuditForRequest(ctx)
	if state == nil || handler.db == nil || apiError == nil || status < 400 || status > 599 || serviceErrorIsUpstream(apiError) {
		return
	}
	trace := snapshotUpstreamTrace(ctx.Request.Context())
	stage := serviceErrorStage(state, status, string(apiError.Code))
	// After an upstream attempt only a local dispatch failure (for example no
	// account left after retries) is a service error; the rest is upstream's.
	if trace.accountID > 0 && stage != "dispatch" {
		return
	}
	if !state.recorded.CompareAndSwap(false, true) {
		return
	}
	requestID := trace.RequestID
	if requestID == "" {
		requestID = NewUpstreamSessionUUID()
	}
	transport := "http"
	if serviceErrorWebSocket(ctx, state) {
		transport = "websocket"
	} else if strings.Contains(ctx.Writer.Header().Get("Content-Type"), "text/event-stream") {
		transport = "sse"
	}
	endpoint := ctx.FullPath()
	if endpoint == "" {
		endpoint = ctx.Request.URL.Path
	}
	model := ctx.GetString("x-model")
	var body []byte
	if transport != "websocket" {
		body = api.GetRawBody(ctx)
	}
	if model == "" {
		model = gjson.GetBytes(body, "model").String()
	}
	event := database.ServiceErrorEvent{
		ID: NewUpstreamSessionUUID(), CreatedAt: time.Now().UTC(), RequestID: requestID,
		StatusCode: status, Code: serviceErrorLabel(string(apiError.Code)), ErrorType: serviceErrorLabel(string(apiError.Type)),
		Message: serviceErrorSafeText(ctx, apiError.Message, 2048), Stage: stage,
		Method: ctx.Request.Method, Endpoint: serviceErrorSafeText(ctx, endpoint, 256), Transport: transport,
		Model: serviceErrorLabel(model), DurationMs: time.Since(state.started).Milliseconds(),
		APIKeyID: state.apiKeyID, APIKeyName: serviceErrorSafeText(ctx, state.apiKeyName, 160),
		ThreadID:   serviceErrorLabel(ctx.GetHeader("Thread-Id")),
		ClientInfo: serviceErrorClientInfo(ctx),
	}
	if stage == "validation" {
		event.ToolProtocol = serviceErrorToolsShape(body)
	}
	if metadata := ctx.GetHeader(codexTurnMetadataHeader); metadata != "" && len(metadata) <= 16384 {
		parsed := gjson.Parse(metadata)
		event.ThreadSource = serviceErrorLabel(parsed.Get("thread_source").String())
		event.RequestKind = serviceErrorLabel(parsed.Get("request_kind").String())
		if event.ThreadID == "" {
			event.ThreadID = serviceErrorLabel(parsed.Get("thread_id").String())
		}
	}
	value, _ := ctx.Get(newAPIPolicyMetaContextKey)
	if policy, valid := value.(verifiedNewAPIPolicyContext); valid && policy.MetaVerified {
		event.NewAPIRequestID = serviceErrorLabel(policy.Identity.RequestID)
		event.NewAPIIdentityVerified = true
		event.NewAPIUserID = serviceErrorSafeText(ctx, policy.Identity.UserID, 160)
		event.NewAPIUserName = serviceErrorSafeText(ctx, policy.Meta.UserName, 160)
	} else {
		// Unverified header: searchable only, never an authorization claim.
		event.NewAPIRequestID = serviceErrorLabel(ctx.GetHeader("X-NewAPI-Request-ID"))
	}
	handler.db.EnqueueServiceError(event)
}

// serviceErrorToolsShape records only the JSON type of an invalid top-level
// tools collection, never its contents. A missing or array value is normal
// and adds nothing.
func serviceErrorToolsShape(body []byte) json.RawMessage {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || tools.IsArray() {
		return nil
	}
	kind := strings.ToLower(tools.Type.String())
	switch {
	case tools.IsObject():
		kind = "object"
	case tools.Type == gjson.True || tools.Type == gjson.False:
		kind = "boolean"
	}
	encoded, _ := json.Marshal(map[string]string{"top_level_tools_type": kind})
	return encoded
}

// serviceErrorClientInfo keeps the client's self-reported identity headers,
// bounded and redacted. Nothing here is verified.
func serviceErrorClientInfo(ctx *gin.Context) map[string]string {
	info := make(map[string]string)
	for key, header := range map[string]string{"user_agent": "User-Agent", "originator": "Originator", "version": "Version"} {
		if value := serviceErrorSafeText(ctx, ctx.GetHeader(header), 256); value != "" {
			info[key] = value
		}
	}
	return info
}
