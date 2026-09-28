package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexOutboundSessionKeepsIsolationAndOtherIdentifiers(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "aligned")
	test.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	test.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "true")
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		test.Run(mode, func(test *testing.T) {
			account := fingerprintAccount(test, mode)
			for _, objectCarrier := range []bool{false, true} {
				headers, body := fingerprintMetadataFixture(test, "root", "child", "parent", "fork", 71)
				headers.Set("X-NewAPI-Session-Fingerprint", "unchanged-signed-identity")
				if objectCarrier {
					body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(headers.Get(codexTurnMetadataHeader)))
				}
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","id":"opaque-id","encrypted_content":"root"},{"role":"user","content":"session_id root"}]`))
				body, _ = sjson.SetBytes(body, "previous_response_id", "previous-root")
				body, _ = sjson.SetBytes(body, "prompt_cache_key", IsolateCodexSessionID(1, "root"))
				originalBody, originalHeaders := bytes.Clone(body), headers.Clone()
				baseline := NewCodexFingerprint(account, headers, body).ApplyBody(body)
				upstream := IsolateCodexSessionID(1, "root")
				fingerprint := NewCodexTransportFingerprint(account, headers, body, upstream)
				prepared := fingerprint.ApplyBody(body)
				outbound := httptest.NewRequest(http.MethodPost, "/responses", nil)
				applyCodexRequestHeaders(outbound, account, "token", upstream, "key", nil, headers, fingerprint)
				require.Equal(test, "root", outbound.Header.Get(codexSessionIDHeader))
				require.Equal(test, "child", outbound.Header.Get(codexThreadIDHeader))
				require.Equal(test, "root", gjson.Get(outbound.Header.Get(codexTurnMetadataHeader), "session_id").String())
				require.Equal(test, "root", gjson.GetBytes(prepared, "client_metadata.session_id").String())
				canonical := gjson.GetBytes(prepared, "client_metadata.x-codex-turn-metadata")
				require.Equal(test, objectCarrier, canonical.IsObject())
				require.Equal(test, "root", gjson.Get(canonical.String(), "session_id").String())
				for _, field := range []string{"thread_id", "parent_thread_id", "forked_from_thread_id", "window_id", "window_number", "context_window_id", "turn_id", "installation_id"} {
					expected := originalBody
					if field == "installation_id" {
						expected = baseline
					}
					require.Equal(test, gjson.Get(gjson.GetBytes(expected, "client_metadata.x-codex-turn-metadata").String(), field).Raw, gjson.Get(canonical.String(), field).Raw, field)
				}
				for _, field := range []string{"input", "previous_response_id", "prompt_cache_key"} {
					require.Equal(test, gjson.GetBytes(originalBody, field).Raw, gjson.GetBytes(prepared, field).Raw, field)
				}
				require.Equal(test, prepared, fingerprint.ApplyBody(prepared))
				repeatedHeaders := outbound.Header.Clone()
				fingerprint.ApplySessionHeaders(repeatedHeaders)
				require.Equal(test, outbound.Header, repeatedHeaders)
				require.Equal(test, originalBody, body)
				require.Equal(test, originalHeaders, headers)
				require.Equal(test, originalHeaders, fingerprint.DownstreamHeaders())
				other := NewCodexTransportFingerprint(account, headers, body, IsolateCodexSessionID(2, "root")).ApplyBody(body)
				require.Equal(test, gjson.GetBytes(prepared, "client_metadata.session_id").String(), gjson.GetBytes(other, "client_metadata.session_id").String())
			}
		})
	}
}

func TestCodexOutboundSessionModesAndMissingIdentity(test *testing.T) {
	account := fingerprintAccount(test, auth.CodexFingerprintModeDevice)
	headers, body := fingerprintMetadataFixture(test, "root", "thread", "", "", 0)
	for _, mode := range []string{"", "preserve", "aligned", "observe", "legacy", "off", "invalid"} {
		test.Run(mode, func(test *testing.T) {
			test.Setenv("CODEX_OUTBOUND_SESSION_MODE", mode)
			fingerprint := NewCodexTransportFingerprint(account, headers, body, "upstream")
			prepared := fingerprint.ApplyBody(body)
			if mode != "observe" && mode != "legacy" && mode != "off" {
				require.True(test, fingerprint.PreservesSessionIdentity())
				require.Equal(test, "root", gjson.GetBytes(prepared, "client_metadata.session_id").String())
			} else {
				require.Equal(test, NewCodexFingerprint(account, headers, body).ApplyBody(body), prepared)
			}
		})
	}
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "aligned")
	for _, session := range []string{"", statelessWebsocketSessionID()} {
		fingerprint := NewCodexTransportFingerprint(account, headers, body, session)
		require.Equal(test, NewCodexFingerprint(account, headers, body).ApplyBody(body), fingerprint.ApplyBody(body))
		outbound := make(http.Header)
		fingerprint.ApplySessionHeaders(outbound)
		require.Equal(test, "root", outbound.Get(codexSessionIDHeader))
	}
	for _, raw := range []string{`{"input":[]}`, `{"client_metadata":{"thread_id":"thread"}}`, `{"client_metadata":{"x-codex-turn-metadata":"not-json"}}`, `{"client_metadata":{"session_id":null,"x-codex-turn-metadata":{"session_id":""}}}`, `{"client_metadata":{"session_id":123,"x-codex-turn-metadata":{"session_id":123}}}`} {
		prepared := NewCodexTransportFingerprint(account, nil, []byte(raw), "upstream").ApplyBody([]byte(raw))
		require.Equal(test, NewCodexFingerprint(account, nil, []byte(raw)).ApplyBody([]byte(raw)), prepared)
	}
	relay := &auth.Account{DBID: 17, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.invalid", APIKey: "private-relay-key"}
	require.Equal(test, NewCodexFingerprint(relay, headers, body).ApplyBody(body), NewCodexTransportFingerprint(relay, headers, body, "upstream").ApplyBody(body))
}

func TestRelaxedOutboundIdentityRequiresPersistentAccount(t *testing.T) {
	for _, mode := range []string{"preserve", "legacy"} {
		for _, available := range []string{"ready", "no_upstream_id", "no_store"} {
			t.Run(mode+"/"+available, func(t *testing.T) {
				t.Setenv("CODEX_OUTBOUND_SESSION_MODE", mode)
				h, account, _, _ := failoverTestSetup(t, false)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
				c, body := failoverTestRequest(t, h)
				ctx := c.Request.Context()
				if available == "no_upstream_id" {
					account.AccountID = ""
				}
				if available == "no_store" {
					ctx = context.Background()
				}
				fingerprint := NewCodexTransportFingerprint(account, c.Request.Header, body, "cache", ctx)
				require.Equal(t, available == "ready", fingerprint.accountIdentityRequested)
				require.NoError(t, fingerprint.ClaimSessionIdentity(ctx, account, "test-user-key"))
			})
		}
	}
}

func TestCodexOutboundSessionHTTPAndCompactFinalBytes(test *testing.T) {
	test.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	type capture struct {
		headers http.Header
		body    []byte
		path    string
	}
	received := make(chan capture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- capture{request.Header.Clone(), readUpstreamRequestBody(request), request.URL.Path}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-test"}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "session-alignment"})
	for _, mode := range []string{"aligned", "observe", "legacy"} {
		for _, endpoint := range []string{"responses", "compact"} {
			test.Run(mode+"/"+endpoint, func(test *testing.T) {
				test.Setenv("CODEX_OUTBOUND_SESSION_MODE", mode)
				account := &auth.Account{DBID: 2791, AccessToken: "private-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice,
					CustomHeaders: map[string]string{"Session-Id": "custom-session", "Session_id": "custom-legacy-session", "Conversation_id": "custom-conversation", "X-Codex-Turn-Metadata": `{"session_id":"custom-session","thread_id":"child"}`}}
				headers, body := fingerprintMetadataFixture(test, "root", "child", "parent", "fork", 72)
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","id":"opaque-id","encrypted_content":"private-encrypted"}]`))
				originalBody, originalHeaders := bytes.Clone(body), headers.Clone()
				upstream := IsolateCodexSessionID(1, "root")
				var response *http.Response
				var err error
				if endpoint == "compact" {
					response, err = ExecuteCompactRequest(context.Background(), account, body, upstream, "", "test-key", nil, headers)
				} else {
					response, err = ExecuteRequest(context.Background(), account, body, upstream, "", "test-key", nil, headers, false)
				}
				require.NoError(test, err)
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(test, err)
				require.NoError(test, response.Body.Close())
				sent := <-received
				if endpoint == "compact" {
					require.Contains(test, sent.path, "/responses/compact")
				}
				require.Equal(test, upstream, gjson.GetBytes(sent.body, "prompt_cache_key").String())
				require.Equal(test, gjson.GetBytes(originalBody, "input").Raw, gjson.GetBytes(sent.body, "input").Raw)
				identity := &outboundIdentityDiagnostic{HTTP: CaptureOutboundIdentityHeaders(sent.headers), Body: captureOutboundIdentityBody(sent.body)}
				{
					require.Equal(test, "root", sent.headers.Get(codexSessionIDHeader))
					require.Empty(test, sent.headers.Get(codexLegacySessionIDHeader))
					require.Empty(test, sent.headers.Get(codexConversationIDHeader))
					if endpoint == "compact" {
						require.False(test, gjson.GetBytes(sent.body, "client_metadata").Exists())
						require.Equal(test, "root", gjson.Get(sent.headers.Get(codexTurnMetadataHeader), "session_id").String())
					} else {
						require.Equal(test, "matched", outboundSessionConsistency(identity))
					}
				}
				require.Equal(test, originalBody, body)
				require.Equal(test, originalHeaders, headers)
			})
		}
	}
}

func TestCodexOutboundSessionPreserveOverridesLegacyHeaderShape(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "aligned")
	test.Setenv("CODEX_SESSION_HEADER_MODE", "legacy")
	headers, body := fingerprintMetadataFixture(test, "root", "thread", "", "", 0)
	fingerprint := NewCodexTransportFingerprint(fingerprintAccount(test, auth.CodexFingerprintModeDevice), headers, body, "upstream")
	outbound := http.Header{}
	outbound.Set(codexSessionIDHeader, "custom-session")
	outbound.Set(codexConversationIDHeader, "custom-conversation")
	fingerprint.ApplySessionHeaders(outbound)
	require.Equal(test, "root", outbound.Get(codexSessionIDHeader))
	require.Empty(test, outbound.Get(codexLegacySessionIDHeader))
	require.Empty(test, outbound.Get(codexConversationIDHeader))
	require.Equal(test, "root", gjson.GetBytes(fingerprint.ApplyBody(body), "client_metadata.session_id").String())
}
