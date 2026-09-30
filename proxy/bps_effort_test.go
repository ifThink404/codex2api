package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSReasoningEffortNormalization(t *testing.T) {
	for requested, want := range map[string]string{
		"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", " HIGH ": "high",
		"none": "low", "minimal": "low",
		"max": "xhigh", "ultra": "xhigh", "x-high": "xhigh", "extra_high": "xhigh", "extra-high": "xhigh",
		"turbo": "medium", "auto": "medium",
	} {
		body, _ := sjson.SetBytes([]byte(`{"model":"gpt-6-astra","input":"hi"}`), "reasoning.effort", requested)
		out, d, err := prepareCodexBPSBodyForProfile(body, "cache", false, false, nil, bpsProfile(auth.BPSExcel))
		require.NoError(t, err, requested)
		require.Equal(t, want, gjson.GetBytes(out, "reasoning_effort").String(), requested)
		require.Equal(t, requested, d.RequestedReasoningEffort)
		require.Equal(t, want, d.SentReasoningEffort)
	}
	out, d, err := prepareCodexBPSBodyForProfile([]byte(`{"model":"gpt-6-astra","input":"hi"}`), "cache", false, false, nil, bpsProfile(auth.BPSExcel))
	require.NoError(t, err)
	require.Equal(t, defaultCodexReasoningEffort, gjson.GetBytes(out, "reasoning_effort").String(), "an absent effort keeps the plugin default")
	require.Contains(t, d.AdaptedFields, "reasoning_effort: absent → low")

	update := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"minimal"}},{"role":"user","content":"hi"}]}`)
	out, d, err = prepareCodexBPSBodyForProfile(update, "cache", false, false, nil, bpsProfile(auth.BPSExcel))
	require.NoError(t, err)
	require.Equal(t, "low", gjson.GetBytes(out, "input.1.reasoning.effort").String())
	require.Contains(t, d.AdaptedFields, "input.configuration_update.reasoning.effort normalized to a BPS tier")
}

func TestBPSAgentIterationCountsToolRounds(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{name: "text input", input: `"hi"`, want: "1"},
		{name: "new user message", input: `[{"role":"user","content":"hi"}]`, want: "1"},
		{name: "one round", input: `[{"role":"user","content":"hi"},{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"a"}]`, want: "2"},
		{name: "parallel results are one round", input: `[{"role":"user","content":"hi"},{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"b"},{"type":"function_call_output","call_id":"a"},{"type":"custom_tool_call_output","call_id":"b"}]`, want: "2"},
		{name: "two rounds", input: `[{"role":"user","content":"hi"},{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"a"},{"type":"function_call","call_id":"b"},{"type":"function_call_output","call_id":"b"}]`, want: "3"},
		{name: "earlier turns do not count", input: `[{"role":"user","content":"old"},{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"a"},{"role":"user","content":"new"}]`, want: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","input":` + tc.input + `}`)
			out, d, err := prepareCodexBPSBodyForProfile(body, "cache", false, false, nil, bpsProfile(auth.BPSExcel))
			require.NoError(t, err)
			require.Equal(t, tc.want, gjson.GetBytes(out, "metadata.agent_iteration").String())
			require.Equal(t, tc.want, d.AgentIteration)
		})
	}
}
