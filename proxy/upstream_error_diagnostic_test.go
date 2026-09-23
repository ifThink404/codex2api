package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
