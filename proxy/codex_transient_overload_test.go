package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func assertCodexTransientOverload(t *testing.T, body []byte, path string) {
	t.Helper()
	require.Equal(t, "rate_limit_exceeded", gjson.GetBytes(body, path+".code").String())
	require.Equal(t, "rate_limit_error", gjson.GetBytes(body, path+".type").String())
	require.Contains(t, gjson.GetBytes(body, path+".message").String(), "Please try again in 30s.")
}

func TestCodexTransientOverloadPreservesRawFailureAndAdvice(t *testing.T) {
	for _, sample := range []struct {
		message string
		delay   time.Duration
	}{
		{"Our servers are currently overloaded. Please try again later.", 30 * time.Second},
		{"Busy. Please try again in 12.5s. Private account@example.invalid", 12500 * time.Millisecond},
		{"Busy. Please try again in 250ms.", 250 * time.Millisecond},
	} {
		raw, err := json.Marshal(gin.H{"error": gin.H{"code": "server_is_overloaded", "type": "service_unavailable_error", "message": sample.message}})
		require.NoError(t, err)
		before := bytes.Clone(raw)
		original := classifyResponseFailedOutcome(raw)
		require.Equal(t, 500, original.logStatusCode)
		require.Contains(t, original.failureMessage, "server_is_overloaded")
		client := publicUpstreamAPIError(nil, raw, 500, "")
		require.Equal(t, "rate_limit_exceeded", string(client.Code))
		require.Equal(t, sample.delay, codexCapacityRetryDelay(client.Message))
		require.NotContains(t, client.Message, "account@example.invalid")
		wire, err := json.Marshal(gin.H{"error": client})
		require.NoError(t, err)
		require.Equal(t, client, publicUpstreamAPIError(nil, wire, 500, ""), "public filtering must not reset retry advice")
		require.Equal(t, before, raw)
	}
}

func TestCodexTransientOverloadHTTPAndCommittedAdvice(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Header("Retry-After", "42")
		require.True(t, writeCodexCapacityError(c, []byte(codexTransientOverloadTestBody), continuousRetryProtocolOpenAI, status))
		require.Equal(t, status, r.Code)
		require.Equal(t, "42", r.Header().Get("Retry-After"))
		require.Contains(t, r.Body.String(), "try again in 42s")
	}
	for _, committed := range []bool{false, true} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Set(codexCapacityResponsesStreamKey, true)
		if committed {
			c.Header("Content-Type", "text/event-stream")
			c.Writer.WriteHeaderNow()
		}
		require.True(t, writeCodexCapacityError(c, []byte(codexTransientOverloadTestBody), continuousRetryProtocolResponses, 500))
		require.Equal(t, 200, r.Code)
		payload := strings.TrimSpace(strings.TrimPrefix(r.Body.String(), "data:"))
		require.Equal(t, "response.failed", gjson.Get(payload, "type").String())
		assertCodexTransientOverload(t, []byte(payload), "response.error")
	}
}

func TestCodexTransientOverloadOnlyRewritesProtocolErrors(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set(codexCapacityResponsesStreamKey, true)
	for _, raw := range []string{`{"type":"error",` + codexTransientOverloadTestBody[1:], `{"type":"response.failed","response":` + codexTransientOverloadTestBody + `}`} {
		out := publicResponseErrorPayload(c, []byte(raw))
		require.Equal(t, "response.failed", gjson.GetBytes(out, "type").String())
		assertCodexTransientOverload(t, out, "response.error")
	}
	// A tool's business error and a hard-stop model-capacity error are not
	// converted into a request retry merely because they mention overload.
	tool := []byte(`{"type":"function_call_output","call_id":"call_1","output":"Our servers are currently overloaded. Please try again later."}`)
	require.JSONEq(t, string(tool), string(publicResponseErrorPayload(c, tool)))
	assertCodexCapacityError(t, publicResponseErrorPayload(c, []byte(`{"type":"response.failed","response":`+codexCapacityTestBody+`}`)), "response.error")
}
