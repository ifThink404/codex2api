package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func configureMultiIdentity(t *testing.T) string {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	config, err := NormalizeCodexUserAgentConfigJSON(`{"mode":"multi","client_kind":"codex-vscode","profiles":{"codex-tui":{"client_version":"0.155.1","os_name":"Windows","terminal":"WindowsTerminal"},"codex-desktop":{"client_version":"0.155.2","app_version":"26.915.31029","os_name":"Mac OS"},"codex-vscode":{"client_version":"0.155.3","app_version":"26.915.31030"},"codex-exec":{"client_version":"0.155.4"},"custom":{"client_name":"Fallback Client","client_version":"0.155.5","os_name":"Linux"}}}`)
	require.NoError(t, err)
	settings := previous
	settings.CodexUserAgentConfig = config
	ApplyRuntimeSettings(settings)
	return config
}

func TestCodexMultiIdentityMatchesOnlyClientPrefix(t *testing.T) {
	configureMultiIdentity(t)
	account := &auth.Account{DBID: 33, AccountID: "test-account", CodexInstallationID: "fixed-account-device"}
	for _, tc := range []struct{ incoming, name, version string }{
		{"codex-tui/9.9.9 (Untrusted Platform)", "codex-tui", "0.155.1"},
		{"codex_cli_rs/0.1.0", "codex-tui", "0.155.1"},
		{"Codex Desktop/0.1.0", "Codex Desktop", "0.155.2"},
		{"codex_work_desktop/0.1.0", "Codex Desktop", "0.155.2"},
		{"codex_vscode/0.1.0", "codex_vscode", "0.155.3"},
		{"codex_exec/0.1.0", "codex_exec", "0.155.4"},
		{"codex-tui-other/0.1.0", "Fallback Client", "0.155.5"},
		{"curl/9.0.0", "Fallback Client", "0.155.5"},
		{"", "Fallback Client", "0.155.5"},
	} {
		t.Run(tc.incoming, func(t *testing.T) {
			headers := http.Header{"User-Agent": {tc.incoming}, "Originator": {"spoofed-origin"}}
			ua, version, origin := ResolveCodexOutboundClientIdentity(account, "key", nil, headers)
			require.Equal(t, tc.name, codexUserAgentClientName(ua))
			require.Equal(t, tc.version, version)
			require.Equal(t, tc.name, origin)
			require.NotContains(t, ua, "Untrusted")
			// The final header pass must keep the same selected profile.
			out := http.Header{"User-Agent": {"custom-override-conflict"}}
			ApplyCodexAccountClientIdentity(out, account, "key", nil, true, headers)
			require.Equal(t, ua, out.Get("User-Agent"))
			require.Equal(t, version, out.Get("Version"))
			require.Equal(t, origin, out.Get("Originator"))
		})
	}
}

func TestCodexMultiIdentityConfigurationAndPreviewAreReadOnly(t *testing.T) {
	raw := configureMultiIdentity(t)
	before := CurrentRuntimeSettings().CodexUserAgentConfig
	preview, err := PreviewCodexUserAgentConfig(raw, "", []int64{33})
	require.NoError(t, err)
	require.Equal(t, "multi", preview.Mode)
	require.Len(t, preview.Samples, 5)
	require.Equal(t, "codex-vscode", preview.Kind)
	require.Equal(t, "codex_vscode", preview.Persona.Originator)
	require.Equal(t, before, CurrentRuntimeSettings().CodexUserAgentConfig)
	var cfg CodexUserAgentConfig
	require.NoError(t, json.Unmarshal([]byte(preview.Normalized), &cfg))
	require.Len(t, cfg.Profiles, 5)
	_, err = NormalizeCodexUserAgentConfigJSON(`{"mode":"multi","profiles":{"custom":{"raw_user_agent":"invalid\r\nHeader:value"}}}`)
	require.Error(t, err)
	_, err = NormalizeCodexUserAgentConfigJSON(`{"mode":"multi","profiles":{"unknown":{}}}`)
	require.Error(t, err)
}

func TestCodexMultiIdentityKeepsAccountOwnedDeviceConsistent(t *testing.T) {
	configureMultiIdentity(t)
	account := &auth.Account{DBID: 33, AccountID: "test-account", CodexInstallationID: "fixed-account-device", CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	for _, client := range []string{"codex-tui", "Codex Desktop", "codex_vscode", "codex_exec", "other"} {
		headers := http.Header{"User-Agent": {client + "/0.1.0"}, "X-Codex-Installation-Id": {"untrusted-device"}}
		body, headers := PrepareCodexOutboundMetadata(account, []byte(`{"input":[],"client_metadata":{"x-codex-installation-id":"untrusted-device","x-codex-turn-metadata":{"installation_id":"untrusted-device"}}}`), headers)
		ApplyCodexFingerprintHeaders(headers, account, headers.Clone())
		body, headers = FinalizeCodexOutboundMetadata(body, headers)
		require.Equal(t, "fixed-account-device", gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String())
		require.Equal(t, "fixed-account-device", headers.Get("X-Codex-Installation-Id"))
		require.NoError(t, ValidateCodexOutboundMetadata(body, headers))
	}
}

func TestCodexMultiIdentityOtherEndpointsAndAccountOverrides(t *testing.T) {
	configureMultiIdentity(t)
	for _, overridden := range []bool{false, true} {
		account := &auth.Account{DBID: 33, AccountID: "test-account", AccessToken: "test-token"}
		name, version := "codex_vscode", "0.155.3"
		if overridden {
			account.CustomHeaders = map[string]string{"User-Agent": "Account Client/1.2.3", "Version": "2.3.4", "Originator": "conflicting-origin"}
			name, version = "Account Client", "2.3.4"
		}
		incoming := http.Header{"User-Agent": {"codex_vscode/9.9.9 (untrusted-platform)"}}
		for _, endpoint := range []string{"auxiliary", "relay", "live"} {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/test", nil)
			switch endpoint {
			case "auxiliary":
				applyCodexAuxiliaryClientHeaders(req, account, "key", nil, incoming, "")
			case "relay":
				applyOpenAIResponsesRequestHeaders(req, account, "key", incoming)
			case "live":
				(&Handler{}).applyLiveUpstreamHeaders(req, account, "", incoming, "key")
			}
			require.Equal(t, name, codexUserAgentClientName(req.Header.Get("User-Agent")), endpoint)
			require.Equal(t, name, req.Header.Get("Originator"), endpoint)
			require.Equal(t, version, req.Header.Get("Version"), endpoint)
			require.Contains(t, req.Header.Get("User-Agent"), "/"+version, endpoint)
			require.NotContains(t, req.Header.Get("User-Agent"), "untrusted", endpoint)
		}
	}
}
