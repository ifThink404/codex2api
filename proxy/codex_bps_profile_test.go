package proxy

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSProfilesExecuteBodyHeadersCompactAndCache(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "profiles.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := withBPSOverride(&auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access"}, true)
	var wire []byte
	var headers http.Header
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		wire, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		headers = r.Header.Clone()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
	})
	sessions := map[string]bool{}
	for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		a.SetCodexBPSOptions(auth.CodexBPSAccountOptions{Profile: profile, Convergence: a.CodexBPSConvergence(), ImageTrim: a.CodexBPSImageTrimEnabled()})
		var session string
		for _, compact := range []bool{false, true} {
			h, body := accountIdentityFixture(t, false, true)
			ctx := withUserAgentAudit(WithCodexIdentityStore(t.Context(), db))
			var resp *http.Response
			if compact {
				resp, err = executeBPSTestCompact(ctx, a, body, "same-session", "", "test-key", nil, h)
			} else {
				resp, err = executeBPSTestRequest(ctx, a, body, "same-session", "", "test-key", nil, h, true)
			}
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			auditedUA, known := upstreamUserAgentAudit(ctx)
			require.True(t, known)
			require.Equal(t, headers.Get("User-Agent"), auditedUA, "all BPS profiles must log the UA sent on the wire")
			require.Equal(t, string(profile), headers.Get("X-Openai-Internal-Basispoints-Client-Editor"))
			if profile == auth.BPSWord {
				require.Empty(t, headers.Get("X-Openai-Internal-Basispoints-Tools-Version-Id"))
			} else {
				require.Equal(t, bpsProfile(profile).toolsVersion, headers.Get("X-Openai-Internal-Basispoints-Tools-Version-Id"))
			}
			require.Equal(t, bpsProfile(profile).toolsVersion, gjson.GetBytes(wire, "metadata.bps_tools_version_id").String())
			require.Equal(t, bpsProfile(profile).runtimeInstructions(), gjson.GetBytes(wire, "input.0.content.0.text").String())
			require.Equal(t, profile, CodexBPSResponseDiagnostic(resp).Profile)
			if profile == auth.BPSSheets {
				require.Empty(t, headers.Get("X-Openai-Internal-Basispoints-Office-Host"))
			}
			if profile == auth.BPSWord {
				require.Empty(t, headers.Get("Session-Id"))
				require.False(t, gjson.GetBytes(wire, "prompt_cache_key").Exists())
				session = gjson.GetBytes(wire, "metadata.task_id").String()
			} else if !compact {
				session = headers.Get("Session-Id")
				require.Equal(t, session, gjson.GetBytes(wire, "prompt_cache_key").String())
			} else {
				require.Equal(t, session, headers.Get("Session-Id"))
			}
		}
		require.False(t, sessions[session])
		sessions[session] = true
	}
	require.Equal(t, codexIdentityDigest("bps-prompt-cache-v1", "account", "cache"), bpsProfileCacheKey("account", "cache", bpsProfile(auth.BPSWord)))
}

func TestBPSProfilesPreserveCallerToolsAndContinuation(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","instructions":"caller instructions","tools":[{"type":"function","name":"read_ranges","parameters":{"type":"object","properties":{"n":{"maximum":9007199254740993}}}},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"note"}]}],"input":[{"type":"function_call","name":"read_ranges","call_id":"call_1","arguments":"{\"n\":9007199254740993}"},{"type":"function_call_output","call_id":"call_1","output":"caller data"},{"type":"compaction","encrypted_content":"opaque-state"}]}`)
	for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		config := bpsProfile(profile)
		for _, compact := range []bool{false, true} {
			wire, d, err := prepareCodexBPSBodyForProfile(body, "cache", compact, false, nil, config)
			require.NoError(t, err)
			require.Equal(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(wire, "input.1.tools").Raw)
			require.Equal(t, "caller instructions", gjson.GetBytes(wire, "input.2.content.0.text").String())
			for i, item := range gjson.GetBytes(body, "input").Array() {
				require.JSONEq(t, item.Raw, gjson.GetBytes(wire, "input").Array()[i+3].Raw)
			}
			d.projection = newBPSResponseProjection(body)
			ctx := context.WithValue(t.Context(), codexBPSDiagnosticKey{}, d)
			for _, event := range []string{
				`{"type":"response.output_item.done","item":{"type":"function_call","name":"read_ranges","call_id":"call_2","arguments":"{\"n\":9007199254740993}"}}`,
				`{"type":"response.output_item.done","item":{"type":"custom_tool_call","name":"note","namespace":"functions","call_id":"call_3","input":"caller code"}}`,
			} {
				projected, err := projectBPSResponse(ctx, []byte(event))
				require.NoError(t, err)
				require.JSONEq(t, event, string(withoutPlaintextArgsMarker(t, projected)))
			}
		}
	}
}

func TestBPSOmitsServiceTierFromRequestsAndConfigurationUpdates(t *testing.T) {
	for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		for _, tier := range []string{"priority", "flex"} {
			body := []byte(`{"model":"gpt-6-astra","service_tier":"` + tier + `","input":[{"type":"configuration_update","service_tier":"` + tier + `","reasoning":{"effort":"max"}},{"role":"user","content":"keep service_tier=priority in this message"},{"type":"function_call_output","call_id":"call-1","output":"{\"service_tier\":\"priority\"}"}]}`)
			original := string(body)
			for _, compact := range []bool{false, true} {
				wire, d, err := prepareCodexBPSBodyForProfile(body, "test-cache", compact, false, http.Header{}, bpsProfile(profile))
				require.NoError(t, err)
				require.False(t, gjson.GetBytes(wire, "service_tier").Exists())
				var updates int
				for _, item := range gjson.GetBytes(wire, "input").Array() {
					if item.Get("type").String() == "configuration_update" {
						updates++
						require.False(t, item.Get("service_tier").Exists())
						require.Equal(t, "xhigh", item.Get("reasoning.effort").String())
					}
					if item.Get("type").String() == "function_call_output" {
						require.JSONEq(t, `{"service_tier":"priority"}`, item.Get("output").String())
					}
				}
				require.Equal(t, 1, updates)
				require.Contains(t, string(wire), "keep service_tier=priority in this message")
				require.Contains(t, d.RemovedFields, "service_tier")
				require.Contains(t, d.RemovedFields, "input.configuration_update.service_tier")
				require.Equal(t, original, string(body), "client's original tier remains available for diagnostics")
			}
		}
	}
}
