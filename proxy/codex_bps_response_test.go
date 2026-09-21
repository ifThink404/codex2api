package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/codex2api/internal/upstreamprivacy"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsProjectionContext(t *testing.T) context.Context {
	t.Helper()
	body := []byte(`{"instructions":"Caller instructions","metadata":{"user_label":"keep"},"tools":[{"type":"function","name":"echo","parameters":{"type":"object","properties":{"n":{"maximum":9007199254740993}}}}],"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"note"}]}]}]}`)
	return context.WithValue(t.Context(), codexBPSDiagnosticKey{}, &CodexBPSDiagnostic{Mode: "bps", projection: newBPSResponseProjection(body)})
}

func TestBPSResponseProjectionRetainsCallerContractAndUsage(t *testing.T) {
	ctx := bpsProjectionContext(t)
	response := `{"object":"response","id":"resp_test","instructions":"Basis Points private runtime name","tools":[{"type":"function","name":"request_user_input_basispoints"}],"metadata":{"bps_tools_version_id":"private","nested":{"bpsPromptBaseOverride":"private"}},"output":[{"type":"function_call","name":"echo","call_id":"call_1","arguments":"{\"bps_tools_version_id\":\"business-value\",\"n\":9007199254740993}"},{"type":"reasoning","encrypted_content":"opaque-original"}],"usage":{"input_tokens":13425,"output_tokens":255,"input_tokens_details":{"cached_tokens":13357}}}`
	for _, raw := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			body := response
			if stream {
				body = "data: {\"type\":\"response.created\",\"response\":" + response + "}\n\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
			}
			var reader io.Reader
			if stream {
				reader = &turnStateStream{ctx: ctx, raw: raw, body: io.NopCloser(strings.NewReader(body)), reader: bufio.NewReader(strings.NewReader(body))}
			} else {
				reader = &responsePrivacyBody{ctx: ctx, raw: raw, statusCode: 200, body: io.NopCloser(strings.NewReader(body))}
			}
			out, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NotContains(t, string(out), "private runtime name")
			require.NotContains(t, string(out), "request_user_input_basispoints")
			require.Contains(t, string(out), "9007199254740993")
			payloads := []string{string(out)}
			if stream {
				payloads = nil
				for _, line := range strings.Split(string(out), "\n") {
					if strings.HasPrefix(line, "data: ") {
						payloads = append(payloads, strings.TrimPrefix(line, "data: "))
					}
				}
			}
			for _, payload := range payloads {
				node := gjson.Parse(payload)
				if stream {
					node = node.Get("response")
				}
				require.Equal(t, "Caller instructions", node.Get("instructions").String())
				require.Equal(t, "echo", node.Get("tools.0.name").String())
				require.Equal(t, "note", node.Get("tools.1.tools.0.name").String())
				require.Equal(t, "keep", node.Get("metadata.user_label").String())
				require.EqualValues(t, 13425, node.Get("usage.input_tokens").Int())
				require.EqualValues(t, 13357, node.Get("usage.input_tokens_details.cached_tokens").Int())
				require.Equal(t, "call_1", node.Get("output.0.call_id").String())
				require.Equal(t, `{"bps_tools_version_id":"business-value","n":9007199254740993}`, node.Get("output.0.arguments").String())
				require.Equal(t, "opaque-original", node.Get("output.1.encrypted_content").String())
			}
		}
	}
	unchanged, err := projectBPSResponse(t.Context(), []byte(response))
	require.NoError(t, err)
	require.Equal(t, response, string(unchanged))
}

func TestBPSProviderSourceTextAcrossEverySplit(t *testing.T) {
	for _, value := range []string{"Basis Points", "request_user_input_basispoints", "/basispoints/api/responses", `\/basispoints\/api\/responses`, `\u0042asis Points`} {
		for split := 1; split < len(value); split++ {
			var frames strings.Builder
			for _, part := range []string{value[:split], value[split:]} {
				data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "delta": part})
				fmt.Fprintf(&frames, "data: %s\n\n", data)
			}
			fmt.Fprint(&frames, "data: [DONE]\n\n")
			r := &turnStateStream{ctx: bpsProjectionContext(t), raw: true, body: io.NopCloser(strings.NewReader(frames.String())), reader: bufio.NewReader(strings.NewReader(frames.String()))}
			out, err := io.ReadAll(r)
			require.NoError(t, err)
			var text strings.Builder
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "data: ") {
					text.WriteString(gjson.Get(strings.TrimPrefix(line, "data: "), "delta").String())
				}
			}
			require.NotEqual(t, value, text.String(), "split=%d", split)
			require.Equal(t, upstreamprivacy.SourceText(value), text.String())
		}
	}
}

func TestBPSMetadataContainersAndUnexpectedProviderTool(t *testing.T) {
	ctx := bpsProjectionContext(t)
	data := []byte(`{"type":"response.metadata","headers":{"X-OpenAI-Internal-Basispoints-Tools-Version-Id":"private","x-codex-turn-state":"real-state"},"client_metadata":"{\"nested\":[{\"bpsToolsVersionId\":\"private\",\"keep\":1}]}"}`)
	out, err := projectBPSResponse(ctx, data)
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(string(out)), "basispoints")
	require.NotContains(t, string(out), "bpsTools")
	require.Contains(t, string(out), "real-state")
	require.Contains(t, string(out), "keep")
	_, err = projectBPSResponse(ctx, []byte(`{"type":"response.output_item.added","item":{"type":"function_call","name":"request_user_input_basispoints","call_id":"c"}}`))
	require.ErrorContains(t, err, "unavailable tool")
}

func TestBPSCallerBusinessPayloadAndDeclaredNamesRemainUsable(t *testing.T) {
	body := []byte(`{"metadata":{"bpsToolsVersionId":"old","nested":[{"bps_tools_version_id":"old","keep":9007199254740993}]},"tools":[{"type":"function","name":"request_user_input_basispoints"}]}`)
	ctx := context.WithValue(t.Context(), codexBPSDiagnosticKey{}, &CodexBPSDiagnostic{projection: newBPSResponseProjection(body)})
	for _, payload := range []string{
		`{"type":"response.output_item.done","item":{"type":"function_call","name":"request_user_input_basispoints","call_id":"call_1","arguments":"{}"}}`,
		`{"type":"response.output_item.done","item":{"type":"function_call_output","call_id":"call_1","output":{"bps_rate":25,"text":"Basis Points business text"}}}`,
		`{"type":"response.output_item.done","item":{"type":"program","code":"return 'Basis Points';","fingerprint":"bps_original"}}`,
	} {
		out, err := projectBPSResponse(ctx, []byte(payload))
		require.NoError(t, err)
		require.JSONEq(t, payload, string(out))
	}
	out, err := projectBPSResponse(ctx, []byte(`{"object":"response","metadata":{"bps_tools_version_id":"provider"}}`))
	require.NoError(t, err)
	require.NotContains(t, string(out), "bps")
	require.Contains(t, string(out), "9007199254740993")
}
