package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestUsageWindowNumbersCaptureActualBody(t *testing.T) {
	for _, step := range []struct{ original, outbound string }{{"0", "0"}, {"47", "0"}, {"48", "1"}, {"18446744073709551615", "2"}} {
		t.Run(step.original, func(t *testing.T) {
			request := transportTestContext()
			body := []byte(fmt.Sprintf(`{"client_metadata":{"x-codex-turn-metadata":{"thread_id":"%s","window_id":"%s:%s","window_number":%s}}}`, continuityTestThread, continuityTestThread, step.original, step.original))
			captureUsageRequestIngress(request, body)
			outbound := []byte(fmt.Sprintf(`{"client_metadata":{"x-codex-turn-metadata":{"thread_id":"%s","window_id":"%s:%s","window_number":%s}}}`, continuityTestThread, continuityTestThread, step.outbound, step.outbound))
			// The captured handshake is deliberately old: each WS frame must use its own body.
			identity := &outboundIdentityDiagnostic{Body: captureOutboundIdentityBody(outbound), WSHandshake: CaptureOutboundIdentityHeaders(http.Header{"X-Codex-Window-Id": {continuityTestThread + ":99"}})}
			upstream, err := json.Marshal(UpstreamTransportDiagnostic{Transport: "websocket", OutboundIdentity: identity})
			require.NoError(t, err)
			input := &database.UsageLogInput{UpstreamDiagnostics: string(upstream)}
			populateUsageRequestDiagnostics(request, input)
			require.Equal(t, step.original, input.WindowNumberOriginal)
			require.Equal(t, step.outbound, input.WindowNumberOutbound)
		})
	}
}

func TestUsageWindowNumbersMissingConflictAndBlocked(t *testing.T) {
	for _, step := range []struct{ body, expected string }{
		{`{"input":"hello"}`, ""},
		{fmt.Sprintf(`{"client_metadata":{"x-codex-turn-metadata":{"window_id":"%s:5","window_number":6}}}`, continuityTestThread), ""},
		{fmt.Sprintf(`{"client_metadata":{"x-codex-turn-metadata":{"window_id":"%s:5","window_number":5}}}`, continuityTestThread), "5"},
	} {
		request := transportTestContext()
		captureUsageRequestIngress(request, []byte(step.body))
		input := &database.UsageLogInput{StatusCode: 400}
		populateUsageRequestDiagnostics(request, input)
		require.Equal(t, step.expected, input.WindowNumberOriginal)
		require.Empty(t, input.WindowNumberOutbound)
	}
	request := transportTestContext()
	request.Request.Header.Set("Connection", "Upgrade")
	request.Request.Header.Set("Upgrade", "websocket")
	request.Request.Header.Set("X-Codex-Window-Id", continuityTestThread+":99")
	captureUsageRequestIngress(request, []byte(`{"input":"current frame without window"}`))
	require.Empty(t, usageRequestDiagnosticState(request).WindowNumberOriginal)
}

func TestUsageBPSBadgeUsesActualOutboundIterationWithoutClientWindow(t *testing.T) {
	for _, tc := range []struct{ policy, metadata, want string }{
		{"bps", `{"agent_iteration":"25","task_id":"task","turn_id":"turn"}`, "25"},
		{"bps", `{"agent_iteration":"1"}`, "1"},
		{"bps", `{"agent_iteration":""}`, ""},
		{"bps", `{"agent_iteration":"-1"}`, ""},
		{"bps", `{"agent_iteration":"0"}`, ""},
		{"bps", `{}`, ""},
		{"", `{"agent_iteration":"25"}`, ""},
	} {
		request := transportTestContext()
		captureUsageRequestIngress(request, []byte(`{"input":"hello","metadata":{"agent_iteration":"999"}}`))
		body, err := sjson.SetBytes([]byte(`{"metadata":`+tc.metadata+`}`), "metadata.bps_tools_version_id", bpsToolsVersion)
		require.NoError(t, err)
		identity := &outboundIdentityDiagnostic{SessionHeaderPolicy: tc.policy, Body: captureOutboundIdentityBody(body)}
		upstream, err := json.Marshal(UpstreamTransportDiagnostic{Transport: "http", OutboundIdentity: identity})
		require.NoError(t, err)
		input := &database.UsageLogInput{UpstreamDiagnostics: string(upstream), BPSAgentIteration: "stale"}
		populateUsageRequestDiagnostics(request, input)
		require.Equal(t, tc.want, input.BPSAgentIteration)
		require.Empty(t, input.WindowNumberOriginal)
		require.Empty(t, input.WindowNumberOutbound)
	}
}
