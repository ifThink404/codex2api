package proxy

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

func TestBPSProbeModeDoesNotChangeAccount(t *testing.T) {
	for _, saved := range []bool{false, true} {
		a := withBPSOverride(&auth.Account{DBID: 7}, saved)
		for _, mode := range []string{"auto", "codex", "bps"} {
			ctx, err := WithCodexTestMode(t.Context(), mode)
			require.NoError(t, err)
			route := resolveBPSTestRoute(ctx, a, []byte(`{"model":"gpt-6-astra"}`), nil, plugins.KindResponses)
			require.Equal(t, mode == "bps" && saved || mode == "auto" && saved, route != nil, "mode %s saved %v", mode, saved)
			enabled, ok := a.TransportPluginOverride(BPSPluginID)
			require.True(t, ok)
			require.Equal(t, saved, enabled, "a probe never writes the account switch")
		}
	}
	ctx, err := WithCodexTestMode(t.Context(), "bps")
	require.NoError(t, err)
	require.Error(t, ValidateCodexTestMode(ctx, &auth.Account{UpstreamType: auth.UpstreamOpenAIResponses}))
	_, err = WithCodexTestMode(t.Context(), "invalid")
	require.Error(t, err)
}

func TestBPSProbeOverrideReachesChosenEndpoint(t *testing.T) {
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "probe.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := withBPSOverride(&auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access"}, true)
	var endpoint string
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		endpoint = r.URL.String()
		return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_probe","output":[]}`))}, nil
	})
	for _, mode := range []string{"bps", "codex"} {
		ctx, err := WithCodexTestMode(WithCodexAccountTestIdentityStore(t.Context(), db, a), mode)
		require.NoError(t, err)
		headers, body := accountIdentityFixture(t, false, true)
		resp, err := executeBPSTestRequest(ctx, a, body, "probe", "", "test-key", nil, headers, false)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if mode == "bps" {
			require.Equal(t, CodexBPSBaseURL+"/responses", endpoint)
		} else {
			require.Equal(t, CodexBaseURL+"/responses", endpoint)
		}
	}
}
