package proxy

import (
	"testing"

	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const bpsLeakyProviderText = "connector secret-arg for user@example.com at bps.openai.com"

func TestBPSErrorBodyKeepsOnlyEnumFieldsAndResetHints(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		wantCode, wantType  string
		wantResetsInSeconds int64
	}{
		{name: "openai envelope", body: `{"error":{"message":"` + bpsLeakyProviderText + `","type":"usage_limit_reached","code":"rate_limit_exceeded","param":"input","resets_in_seconds":120}}`, wantCode: "rate_limit_exceeded", wantType: "usage_limit_reached", wantResetsInSeconds: 120},
		{name: "detail envelope", body: `{"detail":{"error":{"message":"` + bpsLeakyProviderText + `","code":"context_length_exceeded"}}}`, wantCode: "context_length_exceeded"},
		{name: "plain detail", body: `{"detail":"` + bpsLeakyProviderText + `"}`},
		{name: "non-json", body: bpsLeakyProviderText},
		{name: "free-text code", body: `{"error":{"code":"` + bpsLeakyProviderText + `","message":"x"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := scrubBPSErrorBody(429, []byte(tc.body))
			require.NotContains(t, string(out), "secret-arg")
			require.NotContains(t, string(out), "example.com")
			require.NotContains(t, string(out), "param")
			got := gjson.ParseBytes(out)
			require.Equal(t, bpsScrubbedErrorMessage, got.Get("error.message").String())
			require.Equal(t, tc.wantCode, got.Get("error.code").String())
			require.Equal(t, tc.wantType, got.Get("error.type").String())
			require.Equal(t, tc.wantResetsInSeconds, got.Get("error.resets_in_seconds").Int())
		})
	}
}

func TestBPSTerminalEventsAreScrubbed(t *testing.T) {
	for _, tc := range []struct {
		name, event, message string
	}{
		{name: "failed", event: `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"` + bpsLeakyProviderText + `"}}}`, message: bpsScrubbedErrorMessage},
		{name: "completed with failed status", event: `{"type":"response.completed","response":{"id":"resp_1","status":"failed","status_details":{"error":{"message":"` + bpsLeakyProviderText + `","resets_at":1790000000}}}}`, message: bpsScrubbedErrorMessage},
		{name: "incomplete", event: `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"error":{"message":"` + bpsLeakyProviderText + `"}}}`, message: bpsScrubbedIncompleteMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := scrubBPSTerminalEvent([]byte(tc.event))
			require.NoError(t, err)
			require.NotContains(t, string(out), "secret-arg")
			require.NotContains(t, string(out), "status_details")
			got := gjson.ParseBytes(out)
			require.Equal(t, "resp_1", got.Get("response.id").String())
			require.Equal(t, tc.message, got.Get("response.error.message").String())
			if tc.name == "incomplete" {
				require.Equal(t, "max_output_tokens", got.Get("response.incomplete_details.reason").String(), "enum details stay")
			}
		})
	}
	withReset, err := scrubBPSTerminalEvent([]byte(`{"type":"response.completed","response":{"status":"failed","status_details":{"error":{"type":"usage_limit_reached","resets_at":1790000000}}}}`))
	require.NoError(t, err)
	require.EqualValues(t, 1790000000, gjson.GetBytes(withReset, "response.error.resets_at").Int())
	require.Equal(t, "usage_limit_reached", gjson.GetBytes(withReset, "response.error.type").String())

	errorEvent, err := scrubBPSTerminalEvent([]byte(`{"type":"error","code":"invalid_prompt","message":"` + bpsLeakyProviderText + `","param":"input[3]"}`))
	require.NoError(t, err)
	require.NotContains(t, string(errorEvent), "secret-arg")
	require.False(t, gjson.GetBytes(errorEvent, "param").Exists())
	require.Equal(t, "invalid_prompt", gjson.GetBytes(errorEvent, "code").String())

	completed := []byte(`{"type":"response.completed","response":{"status":"completed","error":null,"output":[]}}`)
	out, err := scrubBPSTerminalEvent(completed)
	require.NoError(t, err)
	require.Equal(t, string(completed), string(out))
	delta := []byte(`{"type":"response.output_text.delta","delta":"error: ` + bpsLeakyProviderText + `"}`)
	out, err = scrubBPSTerminalEvent(delta)
	require.NoError(t, err)
	require.Equal(t, string(delta), string(out), "only error events are rewritten")
}

func TestBPSPluginScrubsProviderErrorsInBothTransforms(t *testing.T) {
	env := &plugins.ReqEnv{}
	out, err := bpsPlugin{}.TransformJSON(env, 400, []byte(`{"error":{"message":"`+bpsLeakyProviderText+`","code":"invalid_request_error"}}`))
	require.NoError(t, err)
	require.NotContains(t, string(out), "secret-arg")
	require.Equal(t, "invalid_request_error", gjson.GetBytes(out, "error.code").String())

	body := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"" + bpsLeakyProviderText + "\"}}}\n\n"
	streamed, err := bpsTransformTestBody(bpsProjectionContext(t), body, true)
	require.NoError(t, err)
	require.NotContains(t, string(streamed), "secret-arg")
	require.Contains(t, string(streamed), bpsScrubbedErrorMessage)
}
