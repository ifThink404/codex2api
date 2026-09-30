package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// withoutPlaintextArgsMarker removes the empty encrypted_function_args list the
// projection adds to function calls, so older exact-shape assertions still hold.
func withoutPlaintextArgsMarker(t *testing.T, data []byte) []byte {
	t.Helper()
	for _, path := range []string{"item.encrypted_function_args", "encrypted_function_args"} {
		if marker := gjson.GetBytes(data, path); marker.IsArray() && len(marker.Array()) == 0 {
			var err error
			data, err = sjson.DeleteBytes(data, path)
			require.NoError(t, err)
		}
	}
	return data
}

// Ported from upstream v3.0.5 (d02e5ca8) onto the BPS plugin path.

func TestBPSPluginImageDetailOriginalBecomesHigh(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + bpsTestPNG(t) + `","detail":"original"},{"type":"input_image","image_url":"https://example.test/a.png","detail":"low"}]}]}`)
	out, d, err := prepareCodexBPSBody(body, "cache", false)
	require.NoError(t, err)
	var user gjson.Result
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if item.Get("role").String() == "user" {
			user = item
		}
	}
	require.Equal(t, "high", user.Get("content.0.detail").String())
	require.Equal(t, "low", user.Get("content.1.detail").String())
	require.Equal(t, gjson.GetBytes(body, "input.0.content.0.image_url").String(), user.Get("content.0.image_url").String())
	require.Equal(t, 1, d.Images.DetailNormalized)
	require.Contains(t, d.AdaptedFields, "input image detail original → high")
}

func TestBPSPluginHistoryPlaintextAgentAndReasoningDisplay(t *testing.T) {
	raw := []byte(`{"model":"gpt-6-astra","input":[` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"gAAAAnative","summary":[{"type":"summary_text","text":"s"}],"content":[{"type":"reasoning_text","text":"s"}],"status":"completed"},` +
		`{"type":"agent_message","id":"amsg_1","author":"parent","recipient":"child","content":[{"type":"encrypted_content","encrypted_content":"Return OK when done"},{"type":"encrypted_content","encrypted_content":"gAAAAopaque"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	prepared, _ := PrepareResponsesBody(raw)
	out, _, err := prepareCodexBPSBody(prepared, "cache", false)
	require.NoError(t, err)
	var reasoning, agent gjson.Result
	for _, item := range gjson.GetBytes(out, "input").Array() {
		switch item.Get("type").String() {
		case "reasoning":
			reasoning = item
		case "agent_message":
			agent = item
		}
	}
	require.True(t, reasoning.Exists())
	require.False(t, reasoning.Get("content").Exists(), "display-only reasoning.content must not reach BPS")
	require.False(t, reasoning.Get("status").Exists())
	require.Equal(t, "gAAAAnative", reasoning.Get("encrypted_content").String())
	require.Equal(t, "input_text", agent.Get("content.0.type").String())
	require.Equal(t, "Return OK when done", agent.Get("content.0.text").String())
	require.Equal(t, "encrypted_content", agent.Get("content.1.type").String())
	require.Equal(t, "gAAAAopaque", agent.Get("content.1.encrypted_content").String())
	require.Equal(t, "child", agent.Get("recipient").String())
}

func bpsCompatProjectionContext(t *testing.T) context.Context {
	t.Helper()
	body := []byte(`{"tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}},{"type":"custom","name":"apply_patch"},{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"lookup"}]}]}`)
	return context.WithValue(t.Context(), codexBPSDiagnosticKey{}, &CodexBPSDiagnostic{Mode: "bps", projection: newBPSResponseProjection(body)})
}

func TestBPSPluginFunctionCallsDeclarePlaintextArguments(t *testing.T) {
	ctx := bpsCompatProjectionContext(t)
	out, err := projectBPSResponse(ctx, []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"spawn_agent","call_id":"c1","arguments":"{\"message\":\"go\"}"}}`))
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(out, "item.encrypted_function_args").IsArray())
	require.Len(t, gjson.GetBytes(out, "item.encrypted_function_args").Array(), 0)
	require.Equal(t, `{"message":"go"}`, gjson.GetBytes(out, "item.arguments").String())

	out, err = projectBPSResponse(ctx, []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"spawn_agent","call_id":"c2","arguments":"{}","encrypted_function_args":["message"]}}`))
	require.NoError(t, err)
	require.Equal(t, `["message"]`, gjson.GetBytes(out, "item.encrypted_function_args").Raw)

	out, err = projectBPSResponse(ctx, []byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","name":"apply_patch","call_id":"c3","input":"*** Begin Patch"}}`))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "item.encrypted_function_args").Exists())
}

func TestBPSPluginRepairsMislabeledCustomToolCalls(t *testing.T) {
	ctx := bpsCompatProjectionContext(t)
	cases := []struct {
		name, item, wantType, wantArgs string
	}{
		{"object input", `{"type":"custom_tool_call","name":"spawn_agent","call_id":"c","input":"{\"message\":\"go\"}"}`, "function_call", `{"message":"go"}`},
		{"envelope", `{"type":"custom_tool_call","name":"spawn_agent","call_id":"c","input":"{\"name\":\"spawn_agent\",\"arguments\":{\"message\":\"go\"}}"}`, "function_call", `{"message":"go"}`},
		{"string envelope", `{"type":"custom_tool_call","name":"spawn_agent","call_id":"c","input":"{\"name\":\"spawn_agent\",\"args\":\"{\\\"message\\\":\\\"go\\\"}\"}"}`, "function_call", `{"message":"go"}`},
		{"namespaced", `{"type":"custom_tool_call","name":"mcp.lookup","call_id":"c","input":"{\"q\":1}"}`, "function_call", `{"q":1}`},
		{"other tool envelope", `{"type":"custom_tool_call","name":"spawn_agent","call_id":"c","input":"{\"name\":\"other\",\"arguments\":{}}"}`, "custom_tool_call", ""},
		{"script", `{"type":"custom_tool_call","name":"spawn_agent","call_id":"c","input":"print(1)"}`, "custom_tool_call", ""},
		{"array", `{"type":"custom_tool_call","name":"spawn_agent","call_id":"c","input":"[1]"}`, "custom_tool_call", ""},
		{"two values", `{"type":"custom_tool_call","name":"spawn_agent","call_id":"c","input":"{} {}"}`, "custom_tool_call", ""},
		{"real custom tool", `{"type":"custom_tool_call","name":"apply_patch","call_id":"c","input":"{\"a\":1}"}`, "custom_tool_call", ""},
		{"undeclared", `{"type":"custom_tool_call","name":"unknown","call_id":"c","input":"{\"a\":1}"}`, "custom_tool_call", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := projectBPSResponse(ctx, []byte(`{"type":"response.output_item.done","item":`+tc.item+`}`))
			require.NoError(t, err)
			item := gjson.GetBytes(out, "item")
			require.Equal(t, tc.wantType, item.Get("type").String())
			require.Equal(t, "c", item.Get("call_id").String())
			if tc.wantType == "function_call" {
				require.JSONEq(t, tc.wantArgs, item.Get("arguments").String())
				require.False(t, item.Get("input").Exists())
			} else {
				require.Equal(t, gjson.Get(tc.item, "input").String(), item.Get("input").String())
			}
		})
	}
}

// Ported from upstream v3.0.5 (af602f52) as the plugin config key
// cache_creation_as_input.
func TestBPSPluginCacheCreationAsInput(t *testing.T) {
	previous := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previous) })
	usage := `{"input_tokens":40000,"output_tokens":20,"total_tokens":40020,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":22500},"cache_creation_input_tokens":22500,"cache_creation":{"ephemeral_5m_input_tokens":22500}}`
	event := []byte(`{"type":"response.completed","response":{"object":"response","id":"r","usage":` + usage + `}}`)
	project := func(on, compact bool) gjson.Result {
		cfg := previous
		cfg.CacheCreationAsInput = on
		storeBPSConfig(cfg)
		d := &CodexBPSDiagnostic{Mode: "bps", Compact: compact, projection: newBPSResponseProjection(nil)}
		out, err := projectBPSResponse(context.WithValue(t.Context(), codexBPSDiagnosticKey{}, d), event)
		require.NoError(t, err)
		if !compact {
			require.Equal(t, on, d.Usage.CacheWriteAsInput)
		}
		return gjson.GetBytes(out, "response.usage")
	}
	off := project(false, false)
	require.Positive(t, off.Get("input_tokens_details.cache_write_tokens").Int(), "off by default: the counter is reported")
	require.EqualValues(t, 22500, off.Get("cache_creation_input_tokens").Int())

	on := project(true, false)
	require.Equal(t, off.Get("input_tokens").Int(), on.Get("input_tokens").Int(), "input_tokens is kept")
	require.Equal(t, off.Get("total_tokens").Int(), on.Get("total_tokens").Int())
	for _, path := range []string{"input_tokens_details.cache_write_tokens", "cache_creation_input_tokens", "cache_creation.ephemeral_5m_input_tokens"} {
		require.True(t, on.Get(path).Exists(), path)
		require.Zero(t, on.Get(path).Int(), path)
	}
	require.False(t, on.Get("cache_write_input_tokens").Exists(), "absent counters stay absent")
	require.False(t, on.Get("cache_creation.ephemeral_1h_input_tokens").Exists())

	compact := project(true, true)
	require.EqualValues(t, 40000, compact.Get("input_tokens").Int(), "compact usage is not re-estimated")
	require.Zero(t, compact.Get("input_tokens_details.cache_write_tokens").Int())
}
