package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestAuxiliaryClassifierIdentityAndExtensions(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	h := newWindowAuthorizationHandler(t)
	ctx := WithCodexIdentityStore(context.Background(), h.db)
	const root = accountIdentitySampleRoot
	meta := map[string]any{"session_id": root, "thread_id": root, "guardian_classifier_source_thread_id": root, "thread_source": "guardian_classifier", "externalRef": root, "experiment": "enabled", "valid": "yes"}
	raw, _ := json.Marshal(meta)
	original, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "client_metadata": map[string]any{"session_id": root, "thread_id": root, "x-codex-window-id": root + ":0", "externalRef": root, "x-codex-turn-metadata": string(raw)}})
	for _, relay := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "relay"}[relay], func(t *testing.T) {
			var previousAccount string
			for _, account := range []*auth.Account{{DBID: 42, AccountID: accountIdentitySampleAccount, AccessToken: "test-secret"}, {DBID: 43, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "test-secret-b"}} {
				if relay {
					account.UpstreamType = auth.UpstreamOpenAIResponses
					account.BaseURL = "https://relay.invalid"
					account.APIKey = "test-relay"
				}
				var previous []byte
				for i := 0; i < 2; i++ {
					headers := http.Header{"X-Codex-Guardian": {"classifier"}}
					var body []byte
					mappedCtx := ctx
					if relay {
						var err error
						body, headers, err = prepareRelayOutboundPrivacy(ctx, account, original, headers)
						require.NoError(t, err)
						mappedCtx = withCodexAuxiliaryIdentities(ctx, original, body)
					} else {
						body, headers = PrepareCodexOutboundMetadata(account, original, headers)
						fp := NewCodexTransportFingerprint(account, headers, body, "cache", ctx)
						require.NoError(t, fp.ClaimSessionIdentity(ctx, account, "caller"))
						body = fp.ApplyBody(body)
						mappedCtx = fp.WithAccountIdentityDiagnostic(ctx)
					}
					var err error
					body, err = PrepareCodexFunctionalFields(mappedCtx, account, body, headers, "caller")
					require.NoError(t, err)
					body, headers = FinalizeCodexOutboundMetadata(body, headers)
					require.NoError(t, ValidateCodexOutboundMetadata(body, headers))
					sent := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
					mapped := sent.Get("session_id").String()
					require.NotEmpty(t, mapped)
					require.NotEqual(t, root, mapped)
					for _, field := range []string{"thread_id", "guardian_classifier_source_thread_id", "externalRef"} {
						require.Equal(t, mapped, sent.Get(field).String(), field)
					}
					require.Equal(t, mapped, gjson.GetBytes(body, "client_metadata.externalRef").String())
					require.Equal(t, "enabled", sent.Get("experiment").String())
					require.Equal(t, "yes", sent.Get("valid").String())
					require.False(t, sent.Get("window_id").Exists())
					require.False(t, sent.Get("installation_id").Exists())
					require.False(t, sent.Get("request_kind").Exists())
					require.Equal(t, mapped+":0", gjson.GetBytes(body, "client_metadata.x-codex-window-id").String())
					require.Equal(t, "classifier", headers.Get("X-Codex-Guardian"))
					if i > 0 {
						require.JSONEq(t, string(previous), string(body))
					} else {
						require.NotEqual(t, previousAccount, mapped)
						previousAccount = mapped
					}
					previous = body
				}
			}
		})
	}
}

func TestAuxiliaryParentResponseBindingAndEcho(t *testing.T) {
	h, account, other, _ := responsePrivacySetup(t)
	first, _, _ := responsePrivacyRequest(t, h, 101, "first", "")
	alias, err := responseIdentityFrom(first.Request.Context()).issue(first.Request.Context(), account, "resp_parent_original")
	require.NoError(t, err)
	next, body, _ := responsePrivacyRequest(t, h, 101, "second", "")
	body, _ = sjson.SetBytes(body, "client_metadata.parent_response_id", alias)
	original := append([]byte(nil), body...)
	for i := 0; i < 2; i++ {
		body, err = prepareResponseIdentityOutbound(next.Request.Context(), account, body)
		require.NoError(t, err)
		require.Equal(t, "resp_parent_original", gjson.GetBytes(body, "client_metadata.parent_response_id").String())
		require.Nil(t, responseIdentityFrom(next.Request.Context()).incoming)
		require.Nil(t, responseIdentityFrom(next.Request.Context()).comparison)
	}
	echo, err := maskResponsePayload(next.Request.Context(), account, []byte(`{"type":"response.completed","response":{"id":"resp_child","client_metadata":{"parent_response_id":"resp_parent_original"},"output":[]}}`), false)
	require.NoError(t, err)
	require.Equal(t, alias, gjson.GetBytes(echo, "response.client_metadata.parent_response_id").String())
	require.NotContains(t, string(echo), "resp_parent_original")
	echo, err = maskResponsePayload(next.Request.Context(), account, []byte(`{"type":"parent.echo","parent_response_id":"resp_parent_original"}`), false)
	require.NoError(t, err)
	require.Equal(t, alias, gjson.GetBytes(echo, "parent_response_id").String())
	_, err = prepareResponseIdentityOutbound(next.Request.Context(), other, body)
	require.Error(t, err)
	newEpoch := context.WithValue(next.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: account.ID(), FailoverCount: 1, OutboundWindowReset: true}})
	_, err = prepareResponseIdentityOutbound(newEpoch, account, original)
	require.Error(t, err, "a return to the same account must not restore a previous generation reference")
	foreign, _, _ := responsePrivacyRequest(t, h, 102, "second", "")
	_, err = prepareResponseIdentityOutbound(foreign.Request.Context(), account, original)
	require.Error(t, err)
	different, _, identity := responsePrivacyRequest(t, h, 101, "second", "")
	identity.affinityID = "different-root"
	h.bindResponseIdentity(different, identity)
	_, err = prepareResponseIdentityOutbound(different.Request.Context(), account, original)
	require.Error(t, err)
	for _, value := range []any{"resp_unknown", " " + alias, 42, map[string]any{"id": alias}} {
		invalid, _ := sjson.SetBytes(original, "client_metadata.parent_response_id", value)
		_, err = prepareResponseIdentityOutbound(next.Request.Context(), account, invalid)
		require.Error(t, err)
	}
}

func TestAuxiliaryReusesProjectAndResponseMappings(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "mixed-metadata", "")
	ctx := WithCodexIdentityStore(c.Request.Context(), h.db)
	parent, err := responseIdentityFrom(ctx).issue(ctx, account, "resp_parent_mixed")
	require.NoError(t, err)
	body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "client_metadata": map[string]any{"project_id": projectTestID, "parent_response_id": parent, "x-codex-turn-metadata": map[string]any{"project_id": projectTestID, "externalProject": projectTestID, "externalParent": parent}}})
	ctx, body, err = PrepareCodexProjectOutbound(ctx, account, body, nil)
	require.NoError(t, err)
	body, err = prepareResponseIdentityOutbound(ctx, account, body)
	require.NoError(t, err)
	body, headers := PrepareCodexOutboundMetadata(account, body, nil)
	body, err = PrepareCodexFunctionalFields(ctx, account, body, headers, "caller")
	require.NoError(t, err)
	body, headers = FinalizeCodexOutboundMetadata(body, headers, ctx)
	require.NoError(t, ValidateCodexOutboundMetadata(body, headers))
	meta := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
	project := gjson.GetBytes(body, "client_metadata.project_id").String()
	require.NotEmpty(t, project)
	require.NotEqual(t, projectTestID, project)
	require.Equal(t, project, meta.Get("externalProject").String())
	require.Equal(t, "resp_parent_mixed", meta.Get("externalParent").String())
	echo, _ := json.Marshal(map[string]any{"id": "resp_mixed_echo", "object": "response", "client_metadata": map[string]any{"externalProject": project, "externalParent": "resp_parent_mixed"}})
	restored, err := maskResponsePayload(ctx, account, echo, true)
	require.NoError(t, err)
	require.Equal(t, projectTestID, gjson.GetBytes(restored, "client_metadata.externalProject").String())
	require.Equal(t, parent, gjson.GetBytes(restored, "client_metadata.externalParent").String())
}

func TestAuxiliaryExtensionEchoTraceAndSanitization(t *testing.T) {
	h, account, other, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "auxiliary", "")
	ctx := WithCodexIdentityStore(c.Request.Context(), h.db)
	const ref = "01a09303-49f4-7b53-b545-920f29610317"
	const trace = "00-11223344556677881122334455667788-1122334455667788-01"
	meta := map[string]any{"experiment": "enabled", "externalRef": ref, "encoded": `{"account_id":3268,"session_id":"` + ref + `","token":"secret","x-codex-turn-state":"state","flags":[],"count":5,"valid":"yes"}`}
	raw, _ := json.Marshal(meta)
	original, _ := json.Marshal(map[string]any{"client_metadata": map[string]any{"externalRef": ref, "x-codex-turn-metadata": string(raw), "ws_request_header_traceparent": trace, "ws_request_header_tracestate": "vendor=original-trace", "x-codex-ws-stream-request-start-ms": "1790000000123"}, "input": []any{map[string]any{"role": "user", "content": "unchanged " + ref}}})
	run := func(account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header) {
		out, heads := PrepareCodexOutboundMetadata(account, body, headers)
		var err error
		out, err = PrepareCodexFunctionalFields(ctx, account, out, heads, "caller")
		require.NoError(t, err)
		return FinalizeCodexOutboundMetadata(out, heads)
	}
	out, heads := run(account, original, http.Header{"Traceparent": {trace}, "Tracestate": {"vendor=original-trace"}})
	metaOut := diagnosticMetadataObject(gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata"))
	mapped := metaOut.Get("externalRef").String()
	require.True(t, strings.HasPrefix(mapped, "meta_"))
	require.NotEqual(t, ref, mapped)
	require.Equal(t, mapped, gjson.GetBytes(out, "client_metadata.externalRef").String())
	encoded := gjson.Parse(metaOut.Get("encoded").String())
	require.Equal(t, mapped, encoded.Get("session_id").String())
	require.True(t, strings.HasPrefix(encoded.Get("account_id").String(), "meta_"))
	require.False(t, encoded.Get("token").Exists())
	require.False(t, encoded.Get("x-codex-turn-state").Exists())
	require.Equal(t, "[]", encoded.Get("flags").Raw)
	require.Equal(t, int64(5), encoded.Get("count").Int())
	require.Equal(t, "yes", encoded.Get("valid").String())
	require.Equal(t, gjson.GetBytes(original, "input").Raw, gjson.GetBytes(out, "input").Raw)
	require.Equal(t, "1790000000123", gjson.GetBytes(out, "client_metadata.x-codex-ws-stream-request-start-ms").String())
	require.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`, heads.Get("Traceparent"))
	require.NotEqual(t, trace, heads.Get("Traceparent"))
	require.Equal(t, heads.Get("Traceparent"), gjson.GetBytes(out, "client_metadata.ws_request_header_traceparent").String())
	require.NotContains(t, heads.Get("Tracestate"), "original-trace")
	retry, retryHeads := run(account, out, heads)
	require.JSONEq(t, string(out), string(retry))
	require.Equal(t, heads, retryHeads)
	otherOut, otherHeads := run(other, original, nil)
	require.NotEqual(t, mapped, gjson.GetBytes(otherOut, "client_metadata.externalRef").String())
	require.NotEqual(t, heads.Get("Traceparent"), otherHeads.Get("Traceparent"))
	echo, _ := json.Marshal(map[string]any{"id": "resp_echo", "object": "response", "client_metadata": map[string]any{"externalRef": mapped}})
	restored, err := maskResponsePayload(ctx, account, echo, false)
	require.NoError(t, err)
	require.Equal(t, ref, gjson.GetBytes(restored, "client_metadata.externalRef").String())
	foreign, err := maskResponsePayload(ctx, other, echo, false)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(foreign, "client_metadata.externalRef").Exists())
}

func TestAuxiliaryGuardianControls(t *testing.T) {
	for _, tc := range []struct {
		source, mode, credit string
		keep                 bool
	}{{"user", "", "true", true}, {"guardian_review", "reviewer", "true", false}, {"guardian_classifier", "classifier", "", false}, {"subagent", "", "true", true}, {"user", "reviewer", "true", true}, {"user", "", "false", false}} {
		raw, _ := json.Marshal(map[string]any{"client_metadata": map[string]any{"guardian_credits_requested": tc.credit, "x-codex-turn-metadata": map[string]any{"thread_source": tc.source}}})
		out, headers := PrepareCodexOutboundMetadata(&auth.Account{DBID: 42}, raw, http.Header{"X-Codex-Guardian": {tc.mode}})
		require.Equal(t, tc.keep, gjson.GetBytes(out, "client_metadata.guardian_credits_requested").Exists())
		if tc.source == "user" {
			require.Empty(t, headers.Get("X-Codex-Guardian"))
		} else {
			require.Equal(t, tc.mode, headers.Get("X-Codex-Guardian"))
		}
	}
}

func TestAuxiliaryOptionalFieldsAndInvalidTrace(t *testing.T) {
	account := &auth.Account{DBID: 42, AccessToken: "test-token"}
	for _, trace := range []string{"", "invalid", "00-00000000000000000000000000000000-0123456789abcdef-01", "00-0123456789abcdef0123456789abcdef-0000000000000000-01"} {
		body := []byte(`{"client_metadata":{"ws_request_header_traceparent":"` + trace + `","x-codex-ws-stream-request-start-ms":"invalid","x-codex-turn-metadata":{"thread_source":"guardian_classifier","window_id":"explicit:2","tenant_id":""}}}`)
		body, headers := PrepareCodexOutboundMetadata(account, body, http.Header{"Traceparent": {"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}})
		body, err := PrepareCodexFunctionalFields(context.Background(), account, body, headers, "caller")
		require.NoError(t, err)
		body, headers = FinalizeCodexOutboundMetadata(body, headers)
		meta := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
		require.Equal(t, "explicit:2", meta.Get("window_id").String(), "an explicit classifier field must not be removed")
		require.Equal(t, "", meta.Get("tenant_id").String())
		require.True(t, meta.Get("tenant_id").Exists())
		require.False(t, gjson.GetBytes(body, "client_metadata.ws_request_header_traceparent").Exists())
		require.Empty(t, headers.Get("Traceparent"))
		require.False(t, gjson.GetBytes(body, "client_metadata.x-codex-ws-stream-request-start-ms").Exists())
	}
	for _, key := range []string{"vendor", "tenant@vendor", "1tenant@vendor", "v*/_-1"} {
		require.True(t, validCodexTraceStateKey(key), key)
	}
	for _, key := range []string{"invalid key", "Upper", "name@", "@vendor", "a@b@c", "a=b", "非英文"} {
		require.False(t, validCodexTraceStateKey(key), key)
	}
}
