package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func transportTestContext() *gin.Context {
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attachUpstreamTrace(request, nil)
	return request
}

func TestTransportDiagnosticRateLimitHeaderAllowlist(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "Fri, 25 Sep 2026 14:30:00 GMT")
	headers.Set("X-Ratelimit-Limit-Tokens", "100000")
	headers.Set("X-Ratelimit-Remaining-Tokens", "0")
	headers.Set("X-Ratelimit-Reset-Tokens", "1m2.5s")
	headers.Set("X-Ratelimit-Remaining-Requests", "Bearer private-secret")
	headers.Set("Set-Cookie", "private-cookie")
	require.Equal(t, map[string]string{
		"retry-after": "Fri, 25 Sep 2026 14:30:00 GMT", "x-ratelimit-limit-tokens": "100000",
		"x-ratelimit-remaining-tokens": "0", "x-ratelimit-reset-tokens": "1m2.5s",
	}, upstreamRateLimitHeaders(headers))
	require.Nil(t, upstreamRateLimitHeaders(http.Header{"Retry-After": {"private-secret"}}))
}

func TestTransportDiagnosticWebSocketSeparatesHandshakeAndTurn(test *testing.T) {
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "http://name:password@proxy.invalid:3128/private?token=secret", true)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.Connection("connection-1", "private-pool", "profile-hash", true, true, time.Now().Add(-time.Minute).UnixNano(), "127.0.0.1:3128", "http://name:password@proxy.invalid:3128/private?token=secret")
	headers := http.Header{}
	headers.Set("X-Oai-Request-Id", "handshake-1")
	headers.Set("X-OpenAI-Authorization-Error", "verification_required")
	headers.Set("X-Error-Json", base64.StdEncoding.EncodeToString([]byte(`{"error":{"code":"identity_verification_required","message":"private"},"token":"secret"}`)))
	headers.Set("Set-Cookie", "credential=secret")
	observer.ResponseHeaders(101, headers, true)
	require.Empty(test, snapshotUpstreamTrace(request.Request.Context()).UpstreamRequestID)
	observer.Phase("ambiguous")
	observer.Event([]byte(`{"type":"error","request_id":"turn-request-1","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"secret"}}`))
	input := &database.UsageLogInput{AccountID: 17, StatusCode: 500, ViaWebsocket: true}
	populateUpstreamTrace(request, input)
	populateUsageRequestDiagnostics(request, input)
	require.Equal(test, "turn-request-1", input.UpstreamRequestID)
	for path, expected := range map[string]string{
		"upstream.send_phase": "after_payload", "upstream.error_source": "upstream_ws", "upstream.error_stage": "ws_event",
		"upstream.handshake_request_id": "handshake-1", "upstream.upstream_request_id": "turn-request-1",
		"upstream.proxy_endpoint": "http://proxy.invalid:3128", "upstream.public_egress_ip_status": "not_observed",
		"upstream.identity_error_code": "identity_verification_required", "upstream.error_code": "server_is_overloaded",
	} {
		require.Equal(test, expected, gjson.Get(input.RequestDiagnostics, path).String(), path)
	}
	for _, forbidden := range []string{"password", "secret", "credential", "private", "Set-Cookie"} {
		require.NotContains(test, input.RequestDiagnostics, forbidden)
	}
	require.True(test, gjson.Get(input.RequestDiagnostics, "upstream.connection_reused").Bool())
}

func TestTransportDiagnosticLateObserverCannotContaminateRetry(test *testing.T) {
	request := transportTestContext()
	account := &auth.Account{DBID: 17}
	beginUpstreamTrace(request.Request.Context(), account, "", true)
	old := UpstreamTransportObserver(request.Request.Context())
	old.Event([]byte(`{"type":"error","request_id":"old","error":{"code":"server_is_overloaded"}}`))
	snapshot := snapshotUpstreamTrace(request.Request.Context())
	beginUpstreamTrace(request.Request.Context(), account, "", true)
	old.Failure("transport", "late_close", 1006)
	old.Event([]byte(`{"type":"error","request_id":"late"}`))
	current := snapshotUpstreamTrace(request.Request.Context())
	require.Empty(test, current.UpstreamRequestID)
	require.Empty(test, current.Transport.ErrorSource)
	require.Equal(test, "before_payload", current.Transport.SendPhase)
	input := &database.UsageLogInput{AccountID: 17}
	snapshot.apply(input)
	require.Equal(test, "old", input.UpstreamRequestID)
	require.Equal(test, "ws_event", gjson.Get(input.UpstreamDiagnostics, "error_stage").String())
}

type diagnosticRoundTripper func(*http.Request) (*http.Response, error)

func (transport diagnosticRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestTransportDiagnosticHTTPWriteBoundary(test *testing.T) {
	for _, readPayload := range []bool{false, true} {
		request := transportTestContext()
		outgoing := httptest.NewRequest(http.MethodPost, "http://upstream.invalid/responses", strings.NewReader("payload"))
		outgoing.RequestURI = ""
		outgoing = outgoing.WithContext(request.Request.Context())
		client := &http.Client{Transport: diagnosticRoundTripper(func(request *http.Request) (*http.Response, error) {
			if readPayload {
				_, _ = io.ReadAll(request.Body)
			}
			return nil, errors.New("connection failed")
		})}
		_, err := doTracedUpstreamRequest(client, outgoing, &auth.Account{DBID: 17}, "")
		require.Error(test, err)
		diagnostic := snapshotUpstreamTrace(request.Request.Context()).Transport
		phase := "before_payload"
		if readPayload {
			phase = "ambiguous"
		}
		require.Equal(test, phase, diagnostic.SendPhase)
		require.Equal(test, "transport", diagnostic.ErrorSource)
	}
}

func TestTransportDiagnosticHTTPResponseRecordsActualPeer(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.ReadAll(request.Body)
		writer.Header().Set("X-OpenAI-Request-Id", "upstream-http-1")
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	request := transportTestContext()
	outgoing, err := http.NewRequestWithContext(request.Request.Context(), http.MethodPost, server.URL, strings.NewReader("payload"))
	require.NoError(test, err)
	response, err := doTracedUpstreamRequest(server.Client(), outgoing, &auth.Account{DBID: 17}, "")
	require.NoError(test, err)
	response.Body.Close()
	diagnostic := snapshotUpstreamTrace(request.Request.Context()).Transport
	require.Equal(test, "after_payload", diagnostic.SendPhase)
	require.Equal(test, "upstream_http", diagnostic.ErrorSource)
	require.Equal(test, 503, diagnostic.HTTPStatus)
	require.Equal(test, "upstream-http-1", diagnostic.UpstreamRequestID)
	require.Equal(test, strings.TrimPrefix(server.URL, "http://"), diagnostic.TCPPeer)
	require.Equal(test, "not_observed", diagnostic.PublicEgressIPStatus)
	UpstreamTransportObserver(context.Background()).Failure("transport", "unused", 0)
}

func TestTransportDiagnosticHTTPStreamFailure(test *testing.T) {
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", false)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.ResponseHeaders(200, http.Header{"X-Request-Id": {"http-stream-1"}}, false)
	stream := "event: error\ndata: {\"error\":{\"code\":\"server_is_overloaded\",\"type\":\"server_error\"}}\n\n"
	body := &tracedResponseBody{ReadCloser: io.NopCloser(strings.NewReader(stream)), observer: observer}
	require.NoError(test, ReadSSEStreamWithEvent(body, func(string, []byte) bool { return true }))
	diagnostic := snapshotUpstreamTrace(request.Request.Context()).Transport
	require.Equal(test, "upstream_sse", diagnostic.ErrorSource)
	require.Equal(test, "sse_event", diagnostic.ErrorStage)
	require.Equal(test, "server_is_overloaded", diagnostic.ErrorCode)
	require.Equal(test, "after_payload", diagnostic.SendPhase)
	require.Equal(test, "http-stream-1", diagnostic.UpstreamRequestID)
	body.ReadCloser = io.NopCloser(&diagnosticFailingReader{})
	_, err := body.Read(make([]byte, 1))
	require.Error(test, err)
	require.Equal(test, "upstream_sse", snapshotUpstreamTrace(request.Request.Context()).Transport.ErrorSource)
}

type diagnosticFailingReader struct{}

func (*diagnosticFailingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestTransportDiagnosticReadFailureAndHandshakeFailure(test *testing.T) {
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", false)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.Phase("after_payload")
	body := &tracedResponseBody{ReadCloser: io.NopCloser(&diagnosticFailingReader{}), observer: observer}
	_, err := body.Read(make([]byte, 1))
	require.ErrorIs(test, err, io.ErrUnexpectedEOF)
	diagnostic := snapshotUpstreamTrace(request.Request.Context()).Transport
	require.Equal(test, "transport", diagnostic.ErrorSource)
	require.Equal(test, "http_body_read", diagnostic.ErrorStage)
	require.Equal(test, "failed", diagnostic.ConnectionState)
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	observer = UpstreamTransportObserver(request.Request.Context())
	observer.ResponseHeaders(429, http.Header{"X-Request-Id": {"handshake-rejected"}}, true)
	observer.FailureIfUnset("gateway", "ws_acquire")
	diagnostic = snapshotUpstreamTrace(request.Request.Context()).Transport
	require.Equal(test, "upstream_http", diagnostic.ErrorSource)
	require.Equal(test, "ws_handshake", diagnostic.ErrorStage)
	require.Equal(test, "before_payload", diagnostic.SendPhase)
	require.Equal(test, 429, diagnostic.HandshakeStatus)
	require.Empty(test, diagnostic.UpstreamRequestID)
}

func TestTransportDiagnosticContinuationErrorNeverRetries(test *testing.T) {
	failure := &Error{Code: "response_context_unavailable", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Retryable: false}
	for _, policy := range []database.ContinuousRetryPolicy{{}, {Enabled: true, CatchAll: true}} {
		require.False(test, isRetryableRequestErrorForContext(context.Background(), failure, policy))
	}
}
