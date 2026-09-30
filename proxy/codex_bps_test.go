package proxy

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSExecutorNormalAndCompact(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "bps.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := withBPSOverride(&auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access", CustomHeaders: map[string]string{"X-Openai-Account-Id": "bad-custom-account", "X-Openai-Internal-Basispoints-Client-Device-Id": "bad-device", "Session-Id": "bad-custom-session"}}, true)
	var sentBody []byte
	var sentHeaders http.Header
	var endpoint string
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-bps-test"}`)), Request: r}, nil
		}
		sentBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		sentHeaders, endpoint = r.Header.Clone(), r.URL.String()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"response-test","model":"gpt-5.6-luna","output":[]}`)), Request: r}, nil
	})
	var session, task, turn string
	for _, compact := range []bool{false, true} {
		headers, body := accountIdentityFixture(t, false, true)
		imageData := bpsTestPNG(t)
		body, _ = sjson.SetBytes(body, "model", "codex-auto-review")
		body, _ = sjson.SetRawBytes(body, "context_management", []byte(`[{"type":"compaction","compact_threshold":98765}]`))
		body, _ = sjson.SetBytes(body, "instructions", "Keep the original instructions.")
		body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"function","name":"echo","parameters":{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993}}}}]`))
		body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"additional_tools","tools":[{"type":"custom","name":"note"}]},{"type":"function_call","id":"fc_history","call_id":"call_history","name":"echo","arguments":"{\"n\":1}"},{"type":"function_call_output","call_id":"call_history","output":"1"},{"type":"reasoning","encrypted_content":"opaque-history"}]`))
		body, _ = sjson.SetBytes(body, "input.2.output", []map[string]string{{"type": "input_text", "text": "1"}, {"type": "input_image", "image_url": "data:application/octet-stream;base64," + imageData, "detail": "original"}})
		for _, field := range []string{"service_tier", "include", "text", "tool_choice", "parallel_tool_calls"} {
			body, _ = sjson.SetBytes(body, field, "unsupported-fixture")
		}
		body, _ = sjson.SetBytes(body, "service_tier", "priority")
		if compact {
			body, _ = sjson.SetBytes(body, "service_tier", "flex")
		}
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "old-native-state")
		headers.Set("X-Codex-Turn-State", "old-native-state")
		c := transportTestContext()
		attachUserAgentAudit(c)
		c.Request = c.Request.WithContext(WithCodexIdentityStore(c.Request.Context(), db))
		var resp *http.Response
		if compact {
			resp, err = executeBPSTestCompact(c.Request.Context(), a, body, "cache", "", "test-key", nil, headers)
		} else {
			// Force WS in the caller. The BPS route must still use HTTP/SSE.
			resp, err = executeBPSTestRequest(c.Request.Context(), a, body, "cache", "", "test-key", nil, headers, true)
		}
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.True(t, strings.HasPrefix(endpoint, CodexBPSBaseURL+"/responses"))
		require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(sentBody, "model").String())
		require.Equal(t, "codex-auto-review", gjson.GetBytes(body, "model").String())
		require.False(t, gjson.GetBytes(sentBody, "service_tier").Exists(), "BPS must not send the caller's priority/flex tier")
		require.Equal(t, "Bearer test-access", sentHeaders.Get("Authorization"))
		require.Equal(t, a.AccountID, sentHeaders.Get("Chatgpt-Account-Id"))
		require.Equal(t, a.AccountID, sentHeaders.Get("X-Openai-Account-Id"))
		require.Empty(t, sentHeaders.Get("X-Openai-Internal-Basispoints-Client-Device-Id"))
		require.Empty(t, sentHeaders.Get(codexTurnStateHeader))
		require.Empty(t, sentHeaders.Get(codexTurnMetadataHeader))
		for _, raw := range []string{accountIdentitySampleRoot, "bad-custom", "bad-device", "old-native-state", "9dcfc09b-4e8b-4e25-9052-fefb87224807"} {
			require.NotContains(t, string(sentBody), raw)
		}
		require.Contains(t, string(sentBody), "9007199254740993")
		require.Equal(t, bpsCallerRuntimeInstructions, gjson.GetBytes(sentBody, "input.0.content.0.text").String())
		require.Equal(t, "Keep the original instructions.", gjson.GetBytes(sentBody, "input.2.content.0.text").String())
		require.Equal(t, "note", gjson.GetBytes(sentBody, "input.3.tools.0.name").String())
		require.Equal(t, "call_history", gjson.GetBytes(sentBody, "input.4.call_id").String())
		require.Equal(t, "call_history", gjson.GetBytes(sentBody, "input.5.call_id").String())
		require.Equal(t, "input_text", gjson.GetBytes(sentBody, "input.5.output.1.type").String())
		require.Equal(t, "file-bps-test", gjson.GetBytes(sentBody, "input.6.content.1.file_id").String())
		require.Equal(t, "high", gjson.GetBytes(sentBody, "input.6.content.1.detail").String())
		require.Equal(t, "opaque-history", gjson.GetBytes(sentBody, "input.7.encrypted_content").String())
		require.NotContains(t, string(sentBody), imageData)
		if task == "" {
			session = sentHeaders.Get("Session-Id")
			task = gjson.GetBytes(sentBody, "metadata.task_id").String()
			turn = gjson.GetBytes(sentBody, "metadata.turn_id").String()
		}
		require.Equal(t, session, sentHeaders.Get("Session-Id"))
		require.Equal(t, task, gjson.GetBytes(sentBody, "metadata.task_id").String())
		require.Equal(t, turn, gjson.GetBytes(sentBody, "metadata.turn_id").String())
		if compact {
			require.Equal(t, CodexBPSBaseURL+"/responses/compact", endpoint)
			require.Len(t, gjson.ParseBytes(sentBody).Map(), 3)
		} else {
			require.False(t, gjson.GetBytes(sentBody, "prompt_cache_key").Exists())
			require.EqualValues(t, 98765, gjson.GetBytes(sentBody, "context_management.0.compact_threshold").Int())
			require.True(t, gjson.GetBytes(sentBody, "stream").Bool())
		}
		d := bpsTraceTransport(c.Request.Context())
		require.NotNil(t, d)
		var usage database.UsageLogInput
		populateUserAgentMetaFromRequest(c, &usage)
		require.Equal(t, defaultBPSWordUserAgent, sentHeaders.Get("User-Agent"))
		require.Equal(t, sentHeaders.Get("User-Agent"), usage.UpstreamUserAgent, "usage summary must match the final Word request, not its temporary Codex profile")
		require.Equal(t, "codex-auto-review", d.BPS.RequestedModel)
		require.Equal(t, 1, d.BPS.Images.MIMENormalized)
		require.Equal(t, "gpt-5.6-luna", d.BPS.SentModel)
		require.Contains(t, d.BPS.AdaptedFields, "model: codex-auto-review → gpt-5.6-luna")
		require.Contains(t, d.BPS.RemovedFields, "tool_choice")
		require.Contains(t, d.BPS.RemovedFields, "service_tier")
		require.NotContains(t, string(sentBody), "bps_compat")
	}
}
