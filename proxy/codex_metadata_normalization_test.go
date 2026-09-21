package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexMetadataConflictsNormalizeAcrossFingerprintModes(test *testing.T) {
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		for _, object := range []bool{false, true} {
			test.Run(mode+map[bool]string{false: "/string", true: "/object"}[object], func(test *testing.T) {
				headers, body := fingerprintMetadataFixture(test, "root", "thread-current", "parent-current", "fork-current", 7)
				canonical := gjson.Parse(headers.Get(codexTurnMetadataHeader))
				fields := map[string]string{
					"session_id": "session_id", "thread_id": "thread_id", "window_id": "window_id", "x-codex-window-id": "window_id",
					"installation_id": "installation_id", "x-codex-installation-id": "installation_id", "window_number": "window_number",
					"parent_thread_id": "parent_thread_id", "x-codex-parent-thread-id": "parent_thread_id",
					"forked_from_thread_id": "forked_from_thread_id", "x-codex-forked-from-thread-id": "forked_from_thread_id",
					"context_window_id": "context_window_id", "x-codex-context-window-id": "context_window_id",
					"turn_id": "turn_id", "thread_source": "thread_source", "request_kind": "request_kind",
					"subagent_kind": "subagent_kind", "x-openai-subagent": "subagent_kind",
				}
				for flat := range fields {
					body, _ = sjson.SetBytes(body, "client_metadata."+flat, "stale")
				}
				body, _ = sjson.SetBytes(body, "client_metadata.x-client-request-id", "independent-request")
				body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "current-opaque-state")
				body, _ = sjson.SetBytes(body, "input.0.content", "leave user content unchanged")
				if object {
					body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(canonical.Raw))
				}
				original := bytes.Clone(body)
				resolved := CodexRequestMetadataHeaders(headers, body)
				require.Equal(test, "thread-current", resolved.Get(codexThreadIDHeader))
				require.Equal(test, "thread-current:7", resolved.Get(codexWindowIDHeader))
				fingerprint := NewCodexFingerprint(fingerprintAccount(test, mode), headers, body)
				outbound := fingerprint.ApplyBody(body)
				current := gjson.GetBytes(outbound, "client_metadata.x-codex-turn-metadata")
				require.Equal(test, object, current.IsObject())
				if current.Type == gjson.String {
					current = gjson.Parse(current.String())
				}
				for flat, field := range fields {
					require.Equal(test, current.Get(field).Raw, gjson.GetBytes(outbound, "client_metadata."+flat).Raw, flat)
				}
				require.Equal(test, "independent-request", gjson.GetBytes(outbound, "client_metadata.x-client-request-id").String())
				require.Equal(test, "current-opaque-state", gjson.GetBytes(outbound, "client_metadata.x-codex-turn-state").String())
				require.Equal(test, "leave user content unchanged", gjson.GetBytes(outbound, "input.0.content").String())
				require.Equal(test, original, body)
				require.Equal(test, outbound, fingerprint.ApplyBody(outbound))
			})
		}
	}
}

func TestCodexMetadataNormalizationRespectsMissingAndClearedFields(test *testing.T) {
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":{"thread_id":"current","parent_thread_id":null,"subagent_kind":"","request_kind":"turn"},"thread_id":"old","parent_thread_id":"old-parent","x-codex-parent-thread-id":"old-parent","subagent_kind":"old-agent","x-openai-subagent":"old-agent","x-openai-memgen-request":"true","session_id":"legacy-session","custom":"untouched"}}`)
	outbound := NormalizeCodexRequestMetadata(body)
	require.Equal(test, "current", gjson.GetBytes(outbound, "client_metadata.thread_id").String())
	for _, flat := range []string{"parent_thread_id", "x-codex-parent-thread-id", "subagent_kind", "x-openai-subagent"} {
		require.False(test, gjson.GetBytes(outbound, "client_metadata."+flat).Exists(), flat)
	}
	require.Equal(test, "true", gjson.GetBytes(outbound, "client_metadata.x-openai-memgen-request").String())
	require.Equal(test, "legacy-session", gjson.GetBytes(outbound, "client_metadata.session_id").String())
	require.Equal(test, "untouched", gjson.GetBytes(outbound, "client_metadata.custom").String())
	require.False(test, gjson.GetBytes(outbound, "client_metadata.x-codex-window-id").Exists())
	require.Equal(test, outbound, NormalizeCodexRequestMetadata(outbound))
	headers := CodexRequestMetadataHeaders(http.Header{codexParentThreadIDHeader: {"old-parent"}}, body)
	require.Empty(test, headers.Get(codexParentThreadIDHeader))
	for _, raw := range []string{`{"thread_id":"flat-only"}`, `{"x-codex-turn-metadata":"broken","thread_id":"flat-only"}`, `{"x-codex-turn-metadata":null,"thread_id":"flat-only"}`} {
		unchanged := []byte(`{"client_metadata":` + raw + `}`)
		require.Equal(test, unchanged, NormalizeCodexRequestMetadata(unchanged))
	}
	memory, _ := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.request_kind", "memory")
	require.Equal(test, "true", gjson.GetBytes(NormalizeCodexRequestMetadata(memory), "client_metadata.x-openai-memgen-request").String())
}

func TestCodexMetadataNormalizationPreservesCanonicalEncoding(test *testing.T) {
	canonical := `{"thread_id":"thread-current", "root_turn_id":"root-turn", "parent_turn_id":"parent-turn", "turn_id":"turn"}`
	body, err := json.Marshal(map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": canonical, "root_turn_id": "stale", "parent_turn_id": "stale"}})
	require.NoError(test, err)
	outbound := NormalizeCodexRequestMetadata(body)
	require.Equal(test, canonical, gjson.GetBytes(outbound, "client_metadata.x-codex-turn-metadata").String())
	require.Equal(test, "root-turn", gjson.GetBytes(outbound, "client_metadata.root_turn_id").String())
	require.Equal(test, "parent-turn", gjson.GetBytes(outbound, "client_metadata.parent_turn_id").String())
}

func TestCodexMetadataConflictsNormalizeOnHTTPAndCompact(test *testing.T) {
	test.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	previous := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previous) })
	type capturedRequest struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- capturedRequest{request.Header.Clone(), readUpstreamRequestBody(request)}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "metadata-normalization"})
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		for _, compact := range []bool{false, true} {
			test.Run(mode+map[bool]string{false: "/http", true: "/compact"}[compact], func(test *testing.T) {
				account := &auth.Account{DBID: 1902, AccessToken: "dummy", CodexFingerprintMode: mode}
				headers, body := fingerprintMetadataFixture(test, "root", "child", "parent", "fork", 4)
				for _, field := range []string{"thread_id", "x-codex-window-id", "parent_thread_id", "x-codex-parent-thread-id"} {
					body, _ = sjson.SetBytes(body, "client_metadata."+field, "stale")
				}
				var response *http.Response
				var err error
				if compact {
					response, err = ExecuteCompactRequest(context.Background(), account, body, "isolated-cache", "", "key", nil, headers)
				} else {
					response, err = ExecuteRequest(context.Background(), account, body, "isolated-cache", "", "key", nil, headers, false)
				}
				require.NoError(test, err)
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				require.NoError(test, err)
				sent := <-received
				canonical := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String()
				if compact {
					require.False(test, gjson.GetBytes(sent.body, "client_metadata").Exists())
					canonical = sent.headers.Get(codexTurnMetadataHeader)
				}
				require.Equal(test, canonical, sent.headers.Get(codexTurnMetadataHeader))
				for flat, field := range map[string]string{"thread_id": "thread_id", "x-codex-window-id": "window_id", "parent_thread_id": "parent_thread_id", "x-codex-parent-thread-id": "parent_thread_id"} {
					if !compact {
						require.Equal(test, gjson.Get(canonical, field).String(), gjson.GetBytes(sent.body, "client_metadata."+flat).String(), flat)
					}
				}
				require.Equal(test, "root", sent.headers.Get(codexSessionIDHeader))
				require.Equal(test, "isolated-cache", gjson.GetBytes(sent.body, "prompt_cache_key").String())
			})
		}
	}
}
