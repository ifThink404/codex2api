package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRelaxedOutboundIdentityFallbackIsolation(t *testing.T) {
	h, a, b, _ := failoverTestSetup(t, false)
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	bind := func(user, rootID, leaf string) context.Context {
		c := codexOwnerTestContext(t, h, user, true)
		policy := verifiedNewAPIPolicyContext{Meta: newAPIPolicyMeta{
			RootSessionState: newAPIPolicyRootSessionResolved, RootSessionID: rootID, SessionFingerprint: leaf,
		}}
		bindCodexSessionIdentityFallback(c, requestSessionIdentity{}, requestRootSessionIdentity{
			stable: true, sessionID: rootID, related: leaf != "",
		}, policy, true)
		return c.Request.Context()
	}
	claim := func(ctx context.Context, account *auth.Account) (string, string) {
		body := []byte(`{"input":"hello"}`)
		fingerprint := NewCodexTransportFingerprint(account, nil, body, "", ctx)
		require.NoError(t, fingerprint.ClaimSessionIdentity(ctx, account, "shared-key"))
		require.NotNil(t, fingerprint.accountIdentity)
		require.Equal(t, "signed_newapi", fingerprint.accountIdentityDiagnostic.FallbackSource)
		out := fingerprint.ApplyBody(body)
		headers := make(http.Header)
		fingerprint.ApplySessionHeaders(headers)
		out, headers = FinalizeCodexOutboundMetadata(out, headers)
		require.NoError(t, ValidateCodexOutboundMetadata(out, headers))
		root, thread := headers.Get(codexSessionIDHeader), headers.Get(codexThreadIDHeader)
		parsed, err := uuid.Parse(root)
		require.NoError(t, err)
		require.EqualValues(t, 7, parsed.Version())
		require.Equal(t, root, gjson.GetBytes(out, "client_metadata.session_id").String())
		require.Equal(t, thread, gjson.GetBytes(out, "client_metadata.thread_id").String())
		return root, thread
	}
	firstRoot, firstThread := claim(bind("user-a", continuityTestThread, ""), a)
	resumedRoot, resumedThread := claim(bind("user-a", continuityTestThread, ""), a)
	require.Equal(t, firstRoot, resumedRoot)
	require.Equal(t, firstThread, resumedThread)
	otherUser, _ := claim(bind("user-b", continuityTestThread, ""), a)
	otherAccount, _ := claim(bind("user-a", continuityTestThread, ""), b)
	otherConversation, _ := claim(bind("user-a", accountIdentitySampleContext, ""), a)
	require.NotEqual(t, firstRoot, otherUser)
	require.NotEqual(t, firstRoot, otherAccount)
	require.NotEqual(t, firstRoot, otherConversation)
	childRoot, childOne := claim(bind("user-a", continuityTestThread, "child-one"), a)
	_, childTwo := claim(bind("user-a", continuityTestThread, "child-two"), a)
	_, childRepeated := claim(bind("user-a", continuityTestThread, "child-one"), a)
	require.Equal(t, firstRoot, childRoot)
	require.NotEqual(t, firstThread, childOne)
	require.NotEqual(t, childOne, childTwo)
	require.Equal(t, childOne, childRepeated)
}

func TestRelaxedOutboundIdentityFallbackMetadataAndStrictMode(t *testing.T) {
	h, a, _, _ := failoverTestSetup(t, false)
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	c := codexOwnerTestContext(t, h, "user", true)
	ctx := context.WithValue(c.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{
		key: "authenticated-session-key", preview: true,
		record: database.SessionContinuityRecord{AccountID: a.ID(), FailoverCount: 1, OutboundWindowReset: true},
	})
	var mapped string
	for _, raw := range []string{
		`{"input":"hello"}`,
		`{"client_metadata":{"session_id":null,"thread_id":"","x-codex-turn-metadata":{}}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"\",\"thread_id\":null}"}}`,
	} {
		body := []byte(raw)
		fingerprint := NewCodexTransportFingerprint(a, nil, body, "", ctx)
		require.NoError(t, fingerprint.ClaimSessionIdentity(ctx, a, "shared-key"))
		require.NotNil(t, fingerprint.accountIdentity)
		require.Equal(t, "session_continuity", fingerprint.accountIdentityDiagnostic.FallbackSource)
		out := fingerprint.ApplyBody(body)
		headers := make(http.Header)
		fingerprint.ApplySessionHeaders(headers)
		out, headers = FinalizeCodexOutboundMetadata(out, headers)
		require.NoError(t, ValidateCodexOutboundMetadata(out, headers))
		require.JSONEq(t, string(out), string(fingerprint.ApplyBody(out)), "mapping must be idempotent")
		if mapped == "" {
			mapped = headers.Get(codexSessionIDHeader)
		}
		require.Equal(t, mapped, headers.Get(codexSessionIDHeader))
		require.Equal(t, raw, string(body), "do not mutate inbound metadata")
	}
	// A real client identity still wins over the fallback, and strict mode
	// preserves its prior validation behavior for missing session metadata.
	headers := http.Header{"Session-Id": {accountIdentitySampleRoot}, "Thread-Id": {accountIdentitySampleContext}}
	fingerprint := NewCodexTransportFingerprint(a, headers, []byte(`{}`), "", ctx)
	require.Nil(t, fingerprint.sessionIdentityFallback)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = false; return s })
	fingerprint = NewCodexTransportFingerprint(a, nil, []byte(`{}`), "", ctx)
	require.NoError(t, fingerprint.ClaimSessionIdentity(ctx, a, "shared-key"))
	require.Nil(t, fingerprint.accountIdentity)
	fingerprint = NewCodexTransportFingerprint(a, nil, []byte(`{"client_metadata":{"thread_id":"thread-only"}}`), "", ctx)
	require.Error(t, fingerprint.ClaimSessionIdentity(ctx, a, "shared-key"))
}

func TestRelaxedOutboundIdentityThreadOnly(t *testing.T) {
	h, a, _, _ := failoverTestSetup(t, false)
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	c := codexOwnerTestContext(t, h, "user", true)
	body := []byte(`{"client_metadata":{"thread_id":"sdk-opaque-thread"}}`)
	fingerprint := NewCodexTransportFingerprint(a, nil, body, "", c.Request.Context())
	require.NoError(t, fingerprint.ClaimSessionIdentity(c.Request.Context(), a, "shared-key"))
	require.NotNil(t, fingerprint.accountIdentity)
	require.Equal(t, "thread_id", fingerprint.accountIdentityDiagnostic.FallbackSource)
	out := fingerprint.ApplyBody(body)
	require.Equal(t, gjson.GetBytes(out, "client_metadata.session_id").String(), gjson.GetBytes(out, "client_metadata.thread_id").String())
}
