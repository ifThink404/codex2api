package proxy

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSReasoningEffortCompatibility(t *testing.T) {
	for _, effort := range []string{"max", "xhigh", "high", "low"} {
		body := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"max"},"input":[{"type":"configuration_update","reasoning":{"effort":"max"}},{"role":"user","content":"reasoning.effort=max"},{"type":"function_call_output","output":{"type":"configuration_update","reasoning":{"effort":"max"}}}]}`)
		body, _ = sjson.SetBytes(body, "reasoning.effort", effort)
		original := string(body)
		out, d, err := prepareCodexBPSBody(body, "cache", false)
		require.NoError(t, err)
		want := effort
		if want == "max" {
			want = "xhigh"
		}
		require.Equal(t, want, gjson.GetBytes(out, "reasoning_effort").String())
		require.Equal(t, effort, d.RequestedReasoningEffort)
		require.Equal(t, want, d.SentReasoningEffort)
		require.Equal(t, "xhigh", gjson.GetBytes(out, "input.1.reasoning.effort").String())
		require.Equal(t, "reasoning.effort=max", gjson.GetBytes(out, "input.2.content").String())
		require.Equal(t, "max", gjson.GetBytes(out, "input.3.output.reasoning.effort").String())
		require.Equal(t, original, string(body))
		require.False(t, gjson.GetBytes(out, "requested_reasoning_effort").Exists())
		require.False(t, gjson.GetBytes(out, "sent_reasoning_effort").Exists())
	}
}

func TestBPSMaxMappingAtActualOutboundBoundary(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "effort.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for i, bps := range []bool{false, true} {
		a := withBPSOverride(&auth.Account{DBID: int64(90051 + i), AccountID: accountIdentitySampleAccount, AccessToken: "test-only"}, bps)
		var sent []byte
		installClaudeBoundaryTransport(t, a, func(req *http.Request) (*http.Response, error) {
			sent, err = io.ReadAll(req.Body)
			require.NoError(t, err)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"gpt-6-astra","output":[]}`)), Request: req}, nil
		})
		headers, body := accountIdentityFixture(t, false, true)
		body, _ = sjson.SetBytes(body, "model", "gpt-6-astra")
		body, _ = sjson.SetBytes(body, "reasoning.effort", "max")
		ctx := WithCodexIdentityStore(t.Context(), db)
		resp, err := executeBPSTestRequest(ctx, a, body, "cache", "", "test-key", nil, headers, false)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if bps {
			require.Equal(t, "xhigh", gjson.GetBytes(sent, "reasoning_effort").String())
			d := CodexBPSResponseDiagnostic(resp)
			require.NotNil(t, d)
			require.Equal(t, "max", d.RequestedReasoningEffort)
			require.Equal(t, "xhigh", d.SentReasoningEffort)
			require.Contains(t, d.AdaptedFields, "reasoning_effort: max → xhigh")
		} else {
			require.Equal(t, "max", gjson.GetBytes(sent, "reasoning.effort").String())
		}
	}
}
