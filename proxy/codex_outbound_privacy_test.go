package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOutboundPrivacyMetadataHasOneAuthoritativeSnapshot(t *testing.T) {
	account := &auth.Account{DBID: 42, CodexInstallationID: "account-installation", AccessToken: "test-account-secret"}
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"keep session_id and C:/user/path"}],"tools":[{"type":"function","name":"test","parameters":{"type":"object","properties":{"account_id":{"type":"string"}}}}],"extra_body":{"x-codex-turn-state":"private-extra-state"},"mystery":{"session_id":"private-mystery"},"session_id":"private-top","client_metadata":{"session_id":"stale-flat","x-client-request-id":"independent-request","x_codex_turn_metadata":{"session_id":"stale-alias"},"X-Codex-Turn-Metadata":{"thread_id":"stale-case"},"nested":{"session_id":"private-nested"},"x-codex-turn-metadata":{"session_id":"root","thread_id":"thread","installation_id":"user-installation","parent_turn_id":"parent","nested":{"session_id":"private-inner"},"encoded":"{\"account_id\":\"private-encoded\"}","array":[{"request_id":"private-array"}],"project_id":"private-project","workspaces":{"C:/Users/private/repo":{"cwd":"private-cwd","associated_remote_urls":["private-remote"],"latest_git_commit_hash":"private-commit"}}}}}`)
	headers := http.Header{"Session-Id": {"stale-header"}, "Thread-Id": {"stale-thread"}, "X-Client-Request-Id": {"stale-request"}, "X-Oai-Attestation": {"private-attestation"}}
	originalBody, originalHeaders := bytes.Clone(body), headers.Clone()
	clean, resolved := PrepareCodexOutboundMetadata(account, body, headers)
	var err error
	clean, err = PrepareCodexFunctionalFields(context.Background(), account, clean, resolved, "caller")
	require.NoError(t, err)
	clean, resolved = FinalizeCodexOutboundMetadata(clean, resolved)
	meta := diagnosticMetadataObject(gjson.GetBytes(clean, "client_metadata.x-codex-turn-metadata"))
	require.Equal(t, "root", resolved.Get("Session-Id"))
	require.Equal(t, "root", gjson.GetBytes(clean, "client_metadata.session_id").String())
	require.Equal(t, "thread", resolved.Get("Thread-Id"))
	require.Equal(t, "independent-request", resolved.Get("X-Client-Request-Id"))
	require.Equal(t, "account-installation", meta.Get("installation_id").String())
	require.Equal(t, "account-installation", resolved.Get("X-Codex-Installation-Id"))
	require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(clean, "input").Raw)
	require.Equal(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(clean, "tools").Raw)
	encoded, _ := json.Marshal(resolved)
	for _, original := range []string{"private-extra-state", "private-mystery", "private-top", "private-nested", "private-inner", "private-encoded", "private-array", "private-project", "private-cwd", "private-remote", "private-commit", "private-attestation", "stale-", "user-installation"} {
		require.NotContains(t, string(clean), original)
		require.NotContains(t, string(encoded), original)
	}
	again, againHeaders := PrepareCodexOutboundMetadata(account, clean, resolved)
	require.JSONEq(t, string(clean), string(again))
	require.Equal(t, resolved, againHeaders)
	require.Equal(t, originalBody, body)
	require.Equal(t, originalHeaders, headers)
}

func TestOutboundPrivacyParentTurnAndIndependentRequestAreStable(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	handler := newWindowAuthorizationHandler(t)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	ctx := WithCodexIdentityStore(context.Background(), handler.db)
	headers, body := accountIdentityFixture(t, false, true)
	headers, body = setTurnIdentityTestFields(t, headers, body, turnIdentitySample, turnIdentitySample, false)
	parent := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(t, parent.ClaimSessionIdentity(ctx, account, "key"))
	parentTurn := gjson.GetBytes(parent.ApplyBody(body), "client_metadata.turn_id").String()
	var requestAlias string
	for i := 0; i < 2; i++ {
		childHeaders, childBody := setTurnIdentityTestFields(t, headers, body, turnIdentityChildSample, turnIdentitySample, false)
		childBody, _ = sjson.SetBytes(childBody, "client_metadata.x-codex-turn-metadata.parent_turn_id", turnIdentitySample)
		childBody, _ = sjson.SetBytes(childBody, "client_metadata.x-client-request-id", "independent-client-request")
		childBody, _ = sjson.SetBytes(childBody, "client_metadata.x-codex-turn-metadata.client_request_id", "independent-client-request")
		childBody, childHeaders = PrepareCodexOutboundMetadata(account, childBody, childHeaders)
		child := NewCodexTransportFingerprint(account, childHeaders, childBody, "cache")
		require.NoError(t, child.ClaimSessionIdentity(ctx, account, "key"))
		out := child.ApplyBody(childBody)
		out, final := FinalizeCodexOutboundMetadata(out, child.DownstreamHeaders())
		metadata := diagnosticMetadataObject(gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata"))
		require.Equal(t, parentTurn, metadata.Get("parent_turn_id").String())
		require.Equal(t, parentTurn, metadata.Get("root_turn_id").String())
		require.NotEqual(t, metadata.Get("turn_id").String(), parentTurn)
		request := final.Get("X-Client-Request-Id")
		require.NotEqual(t, "independent-client-request", request)
		require.Equal(t, request, metadata.Get("client_request_id").String())
		require.Equal(t, request, gjson.GetBytes(out, "client_metadata.x-client-request-id").String())
		if i == 0 {
			requestAlias = request
		} else {
			require.Equal(t, requestAlias, request)
		}
		require.Equal(t, out, child.ApplyBody(out), "mapping must not hash an alias again")
	}
}

func TestOutboundPrivacyAttestationAndClientIdentityAreAccountOwned(t *testing.T) {
	account := &auth.Account{DBID: 42, CustomHeaders: map[string]string{"X-Oai-Attestation": "account-attestation", "User-Agent": "codex_cli_rs/0.155.0 (MacOS; arm64)", "Version": "0.156.0", "Originator": "inconsistent-origin"}}
	headers := http.Header{"X-Oai-Attestation": {"client-attestation"}, "User-Agent": {"private-user-agent"}, "Originator": {"private-origin"}, "x-codex-app-version": {"user-version"}}
	ApplyCodexAccountAttestation(headers, account)
	ApplyCodexAccountClientIdentity(headers, account, "", nil, true)
	require.Equal(t, "account-attestation", headers.Get("X-Oai-Attestation"))
	require.Contains(t, headers.Get("User-Agent"), "/0.156.0")
	require.Equal(t, "0.156.0", headers.Get("Version"))
	require.Equal(t, "0.156.0", headers.Get("X-Codex-App-Version"))
	require.NotContains(t, headers, "x-codex-app-version")
	require.Equal(t, "codex_cli_rs", headers.Get("Originator"))
	account.CustomHeaders = nil
	ApplyCodexAccountAttestation(headers, account)
	require.Empty(t, headers.Get("X-Oai-Attestation"))
}

func TestOutboundPrivacyKeepsCompatibleMemoryAndSubagentMarkers(t *testing.T) {
	for _, kind := range []string{"memory", "turn"} {
		body := []byte(`{"client_metadata":{"x-openai-memgen-request":"true","x-openai-subagent":"thread_spawn","x-codex-turn-metadata":{"request_kind":"` + kind + `","subagent_kind":"thread_spawn"}}}`)
		clean, headers := PrepareCodexOutboundMetadata(&auth.Account{DBID: 42}, body, nil)
		clean, headers = FinalizeCodexOutboundMetadata(clean, headers)
		require.NoError(t, ValidateCodexOutboundMetadata(clean, headers))
		require.Equal(t, "collab_spawn", gjson.GetBytes(clean, "client_metadata.x-openai-subagent").String())
		require.Equal(t, "true", gjson.GetBytes(clean, "client_metadata.x-openai-memgen-request").String())
	}
}

func TestOutboundPrivacyDuplicateKeysAndFinalConflictGuard(t *testing.T) {
	for _, raw := range []string{
		`{"client_metadata":{"session_id":"a","session_id":"b"}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"a\",\"thread_id\":\"b\"}"}}`,
		`{"client_metadata":{"session_id":"a"},"client_metadata":{"session_id":"b"}}`,
	} {
		require.Error(t, validateCodexMetadataDuplicates([]byte(raw), false, 0))
	}
	require.NoError(t, validateCodexMetadataDuplicates([]byte(`{"input":[{"arguments":{"session_id":"a","session_id":"b"}}],"client_metadata":{"session_id":"a","session_id":"a"}}`), false, 0))
	body := []byte(`{"client_metadata":{"session_id":"mapped","x-codex-turn-metadata":{"session_id":"mapped"}}}`)
	headers := http.Header{"Session-Id": {"other"}, "X-Codex-Turn-Metadata": {`{"session_id":"mapped"}`}}
	require.Error(t, ValidateCodexOutboundMetadata(body, headers))
	_, headers = FinalizeCodexOutboundMetadata(body, headers)
	require.NoError(t, ValidateCodexOutboundMetadata(body, headers))
	require.Equal(t, "mapped", headers.Get("Session-Id"))
}

func TestOutboundPrivacyRelayScopeAndCapabilityFlag(t *testing.T) {
	account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.invalid", APIKey: "account-key"}
	body := []byte(`{"model":"gpt-6-astra","input":[],"client_metadata":{"session_id":"root","thread_id":"root","x-client-request-id":"root","x-codex-installation-id":"user-device","ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	headers := http.Header{"Authorization": {"Bearer user-1"}, "Idempotency-Key": {"user-request"}, "Openai-Project": {"user-project"}}
	first, firstHeaders, err := prepareRelayOutboundPrivacy(context.Background(), account, body, headers)
	require.NoError(t, err)
	second, secondHeaders, err := prepareRelayOutboundPrivacy(context.Background(), account, body, headers)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, firstHeaders, secondHeaders)
	meta := diagnosticMetadataObject(gjson.GetBytes(first, "client_metadata.x-codex-turn-metadata"))
	require.NotEqual(t, "root", meta.Get("session_id").String())
	require.Equal(t, meta.Get("session_id").String(), firstHeaders.Get("X-Client-Request-Id"))
	require.Empty(t, firstHeaders.Get("OpenAI-Project"))
	require.NotEqual(t, "user-request", firstHeaders.Get("Idempotency-Key"))
	require.Equal(t, "true", gjson.GetBytes(first, codexResponsesLiteWSMetadataPath).String())
	headers.Set("Authorization", "Bearer user-2")
	other, _, err := prepareRelayOutboundPrivacy(context.Background(), account, body, headers)
	require.NoError(t, err)
	require.NotEqual(t, meta.Get("session_id").String(), diagnosticMetadataObject(gjson.GetBytes(other, "client_metadata.x-codex-turn-metadata")).Get("session_id").String())
}
