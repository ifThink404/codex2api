package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/stretchr/testify/require"
)

func upstreamErrorDiagnosticTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder, *Handler) {
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	h := newPromptGuardTestHandler(cfg)
	t.Cleanup(h.store.Stop)
	body := []byte(`{"model":"gpt-6-astra","input":"hello"}`)
	c, recorder := signedNewAPIPolicyContext(t, "original-error", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
	addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{ChannelID: 7, Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce, Provider: "openai", Protocol: "responses"}, true)
	h.primeNewAPIPolicyContext(c, body)
	return c, recorder, h
}

func TestUpstreamErrorDiagnosticRetainsCauseWithoutPublicDisclosure(t *testing.T) {
	c, recorder, _ := upstreamErrorDiagnosticTestContext(t)
	ErrorToGinResponse(c, errors.New("read tcp: connection reset by peer"))
	require.Equal(t, 500, recorder.Code)
	require.Contains(t, recorder.Body.String(), publicUpstreamFailureMessage)
	require.NotContains(t, recorder.Body.String(), "connection reset")
	require.True(t, strings.HasPrefix(recorder.Header().Get("X-Codex2API-Error-Diagnostic"), "v1."), "original failure must reach the bound administrator over a protected carrier")
}

func TestUpstreamErrorDiagnosticNonstandardHTTPMessages(t *testing.T) {
	for name, body := range map[string]string{
		"detail":      `{"detail":"Worker pool exhausted"}`,
		"error":       `{"error":"Worker pool exhausted"}`,
		"nested":      `{"response":{"error":"Worker pool exhausted"}}`,
		"json_string": `"Worker pool exhausted"`,
		"plain_text":  `Worker pool exhausted`,
	} {
		t.Run(name, func(t *testing.T) {
			c, recorder, h := upstreamErrorDiagnosticTestContext(t)
			h.sendUpstreamError(c, 500, []byte(body))
			require.Equal(t, 500, recorder.Code)
			require.Contains(t, recorder.Body.String(), publicUpstreamFailureMessage)
			require.NotContains(t, recorder.Body.String(), "Worker pool exhausted")
			state := currentUpstreamErrorDiagnostic(c)
			require.NotNil(t, state, "must capture the original cause before normalization")
			require.Equal(t, "Worker pool exhausted", state.diagnostic.Message)
			require.True(t, strings.HasPrefix(recorder.Header().Get(upstreamErrorDiagnosticHeader), "v1."))
		})
	}
	for _, body := range []string{`{"unrelated":"do not export this body"}`, `["do not export arbitrary arrays"]`} {
		c, _, h := upstreamErrorDiagnosticTestContext(t)
		h.sendUpstreamError(c, 500, []byte(body))
		require.Nil(t, currentUpstreamErrorDiagnostic(c))
	}
}

func TestUpstreamErrorDiagnosticHTTPAndEventPrivacy(t *testing.T) {
	for _, transport := range []string{"http", "sse", "deferred_sse", "ws", "committed_sse"} {
		t.Run(transport, func(t *testing.T) {
			c, recorder, h := upstreamErrorDiagnosticTestContext(t)
			c.Set("apiKey", "downstream-secret")
			attachUpstreamTrace(c, h.store)
			account := &auth.Account{DBID: 17, AccessToken: "upstream-secret"}
			h.store.AddAccount(account)
			beginUpstreamTrace(c.Request.Context(), account, "", transport == "ws")
			observer := UpstreamTransportObserver(c.Request.Context())
			observer.ResponseHeaders(500, http.Header{"X-Request-Id": []string{"upstream-request"}}, false)
			raw := []byte(`{"error":{"code":"internal_error","type":"server_error","message":"worker failed: upstream-secret downstream-secret at https://user:pass@private.example/path?key=secret 10.0.0.1 Bearer very-secret"}}`)
			var output string
			switch transport {
			case "http":
				h.sendUpstreamError(c, 500, raw)
				output = recorder.Body.String()
			case "sse", "deferred_sse":
				writer := h.newStreamFlushWriter(c, recorder, recorder)
				data := []byte(`{"type":"response.failed","response":` + string(raw) + `}`)
				if transport == "sse" {
					require.NoError(t, writer.WriteSSEData(data))
				} else {
					var pending bytes.Buffer
					_, err := writeDeferredSSEData(writer, &pending, data, true)
					require.NoError(t, err)
					require.Empty(t, recorder.Body.String(), "deferred failed attempts cannot leak a diagnostic before commit")
					require.NoError(t, writer.WriteBytes(pending.Bytes()))
				}
				require.NoError(t, writer.Flush())
				output = recorder.Body.String()
				require.Contains(t, output, ": codex2api_error v1.")
			case "committed_sse":
				publicUpstreamAPIError(c, raw, 500, "")
				c.Writer.WriteHeaderNow()
				require.True(t, writeCommittedResponsesRetryError(c, publicUpstreamFailureMessage))
				output = recorder.Body.String()
				require.Contains(t, output, ": codex2api_error v1.")
			case "ws":
				c.Request.Header.Set("Upgrade", "websocket")
				c.Request.Header.Set("Connection", "Upgrade")
				output = string(publicResponseErrorPayload(c, []byte(`{"type":"response.failed","response":`+string(raw)+`}`)))
				require.NotEmpty(t, gjson.Get(output, "response.error.details.codex2api_error").String())
			}
			state := currentUpstreamErrorDiagnostic(c)
			require.NotNil(t, state)
			require.Contains(t, state.diagnostic.Message, "worker failed")
			require.Equal(t, 500, state.diagnostic.HTTPStatus)
			require.Equal(t, "upstream-request", state.diagnostic.UpstreamRequestID)
			for _, private := range []string{"upstream-secret", "downstream-secret", "private.example", "very-secret", "10.0.0.1", "user:pass"} {
				require.NotContains(t, output, private)
				require.NotContains(t, state.diagnostic.Message, private)
			}
			require.NotContains(t, output, "worker failed")
			// Crossing the public filter twice keeps the original diagnostic.
			publicUpstreamAPIError(c, []byte(`{"error":{"code":"internal_error","message":"`+publicUpstreamFailureMessage+`"}}`), 500, "")
			require.Contains(t, currentUpstreamErrorDiagnostic(c).diagnostic.Message, "worker failed")
			beginUpstreamTrace(c.Request.Context(), account, "", false)
			require.Nil(t, currentUpstreamErrorDiagnostic(c), "a new attempt must not inherit an earlier error")
		})
	}
}

func TestUpstreamErrorDiagnosticUnboundAndSyntheticStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ErrorToGinResponse(c, &Error{Code: ErrorCodeUpstreamTimeout, Message: "read deadline exceeded", Type: ErrorTypeUpstreamError, HTTPStatus: 504, Cause: errors.New("connection lost")})
	require.Equal(t, 504, recorder.Code)
	require.Empty(t, recorder.Header().Get(upstreamErrorDiagnosticHeader))
	state := currentUpstreamErrorDiagnostic(c)
	require.NotNil(t, state)
	require.Zero(t, state.diagnostic.HTTPStatus)
	require.Contains(t, state.diagnostic.Message, "connection lost")
}

func TestUpstreamErrorDiagnosticWireFixture(t *testing.T) {
	d := upstreamErrorDiagnostic{RequestID: "error-fixture", ChannelID: 7, IssuedAt: 1788566400, Message: "read tcp: connection reset by peer", Code: "internal_error", Type: "server_error", Source: "transport", Stage: "ws_read", Transport: "websocket", HandshakeStatus: 101, GatewayRequestID: "gateway-request"}
	encoded, err := sealUpstreamErrorDiagnostic("integration-secret", "42", "test-platform", d, bytes.NewReader(bytes.Repeat([]byte{42}, 12)))
	require.NoError(t, err)
	fixture, err := os.ReadFile("testdata/upstream_error_diagnostic_v1.txt")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(fixture)), encoded)
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	require.NotContains(t, encoded, string(raw))
}

func TestUpstreamErrorDiagnosticDoesNotInventHTTPStatusFromWSAdapter(t *testing.T) {
	c, _, h := upstreamErrorDiagnosticTestContext(t)
	attachUpstreamTrace(c, h.store)
	account := &auth.Account{DBID: 17, AccessToken: "test-token"}
	h.store.AddAccount(account)
	beginUpstreamTrace(c.Request.Context(), account, "", true)
	UpstreamTransportObserver(c.Request.Context()).ResponseHeaders(101, http.Header{}, true)
	publicUpstreamAPIError(c, []byte(`{"error":{"code":"internal_error","message":"worker failed"}}`), 500, "")
	d := currentUpstreamErrorDiagnostic(c).diagnostic
	require.Zero(t, d.HTTPStatus)
	require.Equal(t, 101, d.HandshakeStatus)
}

func TestUpstreamErrorDiagnosticPersistsBeforeRetryAndPublicResponse(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	c, recorder, h := upstreamErrorDiagnosticTestContext(t)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "errors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	h.db = db
	accountID, err := db.InsertOpenAIResponsesAccount(t.Context(), "diagnostic-test", map[string]any{"base_url": "https://example.test", "api_key": "upstream-secret"}, "")
	require.NoError(t, err)
	account := &auth.Account{DBID: accountID, AccessToken: "upstream-secret"}
	h.store.AddAccount(account)
	attachUpstreamTrace(c, h.store)

	bodies := []string{
		`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Token rate exceeded upstream-secret"}}`,
		`{"detail":[{"loc":["body","max_tokens"],"msg":"Input should be at most 65536","type":"less_than_equal","input":"private-prompt","ctx":{"secret":"private-context"}}]}`,
		`{}`,
	}
	for i, status := range []int{429, 422, 200} {
		beginUpstreamTrace(c.Request.Context(), account, "", false)
		headers := http.Header{}
		headers.Set("X-Request-Id", []string{"attempt-429", "attempt-422", "attempt-ok"}[i])
		if status == 429 {
			headers.Set("Retry-After", "7")
			headers.Set("X-Ratelimit-Remaining-Tokens", "0")
		}
		UpstreamTransportObserver(c.Request.Context()).ResponseHeaders(status, headers, false)
		body := []byte(bodies[i])
		if status >= 400 {
			logUpstreamErrorForRequest(c, "/v1/responses", status, "gpt-6-astra", accountID, body)
		}
		h.logUsageForRequest(c, &database.UsageLogInput{AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-6-astra", StatusCode: status, AttemptIndex: i + 1, IsRetryAttempt: status >= 400, ErrorMessage: usageLogErrorMessage(status, body)})
		require.Empty(t, recorder.Header().Get(upstreamErrorDiagnosticHeader), "a failed attempt must not commit headers before retry selection")
		if status == 422 {
			state := currentUpstreamErrorDiagnostic(c)
			require.NotNil(t, state)
			require.NotEmpty(t, state.envelope, "the final original cause must remain available to NewAPI")
			require.Contains(t, state.diagnostic.Message, "max_tokens")
			require.NotContains(t, state.diagnostic.Message, "private-")
		}
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, logs, 3)
	for _, row := range logs {
		detail, err := db.GetUsageRequestDiagnostics(t.Context(), row.ID)
		require.NoError(t, err)
		d := gjson.GetBytes(detail.Diagnostics, "upstream")
		switch row.StatusCode {
		case 429:
			require.Contains(t, d.Get("error_detail.message").String(), "Token rate exceeded")
			require.Equal(t, "attempt-429", d.Get("error_detail.upstream_request_id").String())
			require.Equal(t, "7", d.Get("rate_limit_headers.retry-after").String())
			require.Equal(t, "rate_limit_exceeded", d.Get("error_detail.code").String())
			require.NotContains(t, d.Raw, "upstream-secret")
		case 422:
			require.Contains(t, d.Get("error_detail.message").String(), "max_tokens")
			require.Equal(t, "validation_error", d.Get("error_detail.type").String())
			require.Contains(t, row.ErrorMessage, "Input should be at most 65536")
			require.False(t, d.Get("rate_limit_headers").Exists())
			require.NotContains(t, string(detail.Diagnostics), "private-")
		case 200:
			require.False(t, d.Get("error_detail").Exists(), "success must not inherit either failed attempt")
			require.False(t, d.Get("error_response").Exists())
			require.False(t, d.Get("rate_limit_headers").Exists())
		}
	}
}

func TestUpstreamErrorDiagnosticRecordsMissingExplanation(t *testing.T) {
	for _, body := range []string{"", `{"unexpected":"private-prompt"}`} {
		c := transportTestContext()
		beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)
		UpstreamTransportObserver(c.Request.Context()).ResponseHeaders(429, http.Header{}, false)
		captureUpstreamErrorDiagnostic(c, []byte(body), 429, "upstream_http", "http_response")
		d := snapshotUpstreamTrace(c.Request.Context()).Transport
		require.NotNil(t, d.ErrorResponse)
		require.Equal(t, len(body), d.ErrorResponse.BodyBytes)
		require.False(t, d.ErrorResponse.MessageFound)
		require.Nil(t, d.ErrorDetail)
		encoded, err := json.Marshal(d)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "private-prompt")
	}
}

func TestUpstreamErrorDiagnosticStreamRetryWithoutPolicyIncident(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	enableLooseResponseFailedContinuousRetry(t)
	attempts := looseTTFTFailureThenSuccessEvents("recovered-with-logs")
	attempts[0][2] = `{"type":"response.failed","response":{"status":"failed","status_code":429,"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Token rate exceeded"}}}`
	upstream, calls := newAttemptSequenceSSEServer(t, attempts)
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "stream-errors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.InsertOpenAIResponsesAccount(t.Context(), "diagnostic-test", map[string]any{"base_url": upstream.URL, "api_key": "test-key"}, "")
	require.NoError(t, err)
	h := NewHandler(store, db, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","input":"hello","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachUpstreamTrace(c, store)
	h.Responses(c)
	require.EqualValues(t, 2, calls.Load())
	require.Equal(t, 200, recorder.Code)
	require.Contains(t, recorder.Body.String(), "recovered-with-logs")
	require.NotContains(t, recorder.Body.String(), "Token rate exceeded")
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, logs, 2, "the unsuccessful retry must be retained without a policy incident")
	for _, row := range logs {
		detail, err := db.GetUsageRequestDiagnostics(t.Context(), row.ID)
		require.NoError(t, err)
		if row.StatusCode == 429 {
			require.True(t, row.IsRetryAttempt)
			require.Contains(t, string(detail.Diagnostics), "Token rate exceeded")
			require.EqualValues(t, 200, gjson.GetBytes(detail.Diagnostics, "upstream.error_detail.http_status").Int(), "SSE failure must retain the actual HTTP status")
		} else {
			require.Equal(t, 200, row.StatusCode)
			require.NotContains(t, string(detail.Diagnostics), "Token rate exceeded")
		}
	}
}
