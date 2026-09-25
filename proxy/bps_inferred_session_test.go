package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const inferredOpening = `{"model":"gpt-6-astra","input":[{"role":"user","content":"build a clock"}]}`
const inferredFollowup = `{"model":"gpt-5.6-sol","tools":[{"type":"function","name":"clock"}],"input":[{"role":"user","content":"build a clock"},{"role":"assistant","content":"done"},{"role":"user","content":"make it blue"}]}`

func inferredSessionFixture(t *testing.T, body, user, device, ua string, key int64) *gin.Context {
	t.Helper()
	c := transportTestContext()
	c.Set(contextAPIKeyID, key)
	c.Request.Header.Set("User-Agent", ua)
	if device != "" {
		c.Request.Header.Set(codexInstallationIDHeader, device)
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), transportUserContextKey{}, user))
	identity := resolveRequestSessionIdentity(c.Request.Header, []byte(body))
	bindInferredBPSSession(c, []byte(body), identity, requestRootSessionIdentity{}, verifiedNewAPIPolicyContext{}, false)
	return c
}

func TestInferredBPSConversationScope(t *testing.T) {
	a := &auth.Account{DBID: 1241, AccountID: "account-a", CodexFingerprintMode: auth.CodexFingerprintModeSession}
	seed := func(body, user, device, ua string, key int64) (string, *inferredBPSSessionDiagnostic) {
		c := inferredSessionFixture(t, body, user, device, ua, key)
		return inferredBPSCacheSeed(c.Request.Context(), a, "random-attempt", false)
	}
	first, diagnostic := seed(inferredOpening, "user-a", "device-a", "client/1", 8)
	require.Equal(t, "applied", diagnostic.Result)
	require.True(t, diagnostic.Heuristic)
	next, _ := seed(inferredFollowup, "user-a", "device-a", "client/1", 8)
	require.Equal(t, first, next, "new turns, models and tool declarations must retain the hint")
	for _, change := range []struct {
		body, user, device, ua string
		key                    int64
	}{
		{inferredOpening, "user-b", "device-a", "client/1", 8},
		{inferredOpening, "user-a", "device-b", "client/1", 8},
		{inferredOpening, "user-a", "device-a", "client/2", 8},
		{strings.Replace(inferredOpening, "a clock", "a calendar", 1), "user-a", "device-a", "client/1", 8},
		{inferredOpening, "user-a", "device-a", "client/1", 9},
	} {
		changed, _ := seed(change.body, change.user, change.device, change.ua, change.key)
		require.NotEqual(t, first, changed)
	}
	withoutDevice, absent := seed(inferredOpening, "user-a", "", "Go-http-client/2.0", 8)
	withoutDeviceAgain, _ := seed(inferredFollowup, "user-a", "", "Go-http-client/2.0", 8)
	require.Equal(t, withoutDevice, withoutDeviceAgain)
	require.Equal(t, "user_conversation_prefix", absent.Source)
	require.Equal(t, "unavailable", absent.DeviceSource)
	encoded, err := json.Marshal(absent)
	require.NoError(t, err)
	for _, sensitive := range []string{"build a clock", "Go-http-client", "user-a", withoutDevice} {
		require.NotContains(t, string(encoded), sensitive)
	}
	// Direct keys are separate scopes; unverified user headers confer no identity.
	directA, _ := seed(inferredOpening, "", "device-a", "client/1", 8)
	directB, _ := seed(inferredOpening, "", "device-a", "client/1", 9)
	require.NotEqual(t, directA, directB)
	_, anonymous := seed(inferredOpening, "", "device-a", "client/1", 0)
	require.Equal(t, "authenticated_scope_unavailable", anonymous.Reason)
}

func TestInferredBPSSessionEligibility(t *testing.T) {
	a := &auth.Account{DBID: 1242, CodexFingerprintMode: auth.CodexFingerprintModeSession}
	for _, tc := range []struct {
		name, body, reason string
		root               requestRootSessionIdentity
		identity           requestSessionIdentity
	}{
		{"explicit", inferredOpening, "explicit_identity", requestRootSessionIdentity{}, requestSessionIdentity{explicitUpstreamID: "client-session"}},
		{"root", inferredOpening, "explicit_identity", requestRootSessionIdentity{stable: true}, requestSessionIdentity{}},
		{"conflict", inferredOpening, "identity_conflict", requestRootSessionIdentity{conflict: true}, requestSessionIdentity{}},
		{"related", inferredOpening, "related_or_internal_request", requestRootSessionIdentity{related: true}, requestSessionIdentity{}},
		{"background", inferredOpening, "related_or_internal_request", requestRootSessionIdentity{threadSource: "subagent"}, requestSessionIdentity{}},
		{"continuation", `{"previous_response_id":"resp-1","input":"next"}`, "response_continuation", requestRootSessionIdentity{}, requestSessionIdentity{}},
		{"compact", `{"input":[{"type":"compaction","encrypted_content":"opaque"},{"role":"user","content":"next"}]}`, "compaction_context", requestRootSessionIdentity{}, requestSessionIdentity{}},
		{"truncated", `{"input":[{"role":"assistant","content":"old reply"},{"role":"user","content":"next"}]}`, "conversation_prefix_unavailable", requestRootSessionIdentity{}, requestSessionIdentity{}},
		{"no-user", `{"input":[{"role":"system","content":"system"}]}`, "conversation_prefix_unavailable", requestRootSessionIdentity{}, requestSessionIdentity{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inferredSessionFixture(t, inferredOpening, "user-a", "device-a", "client/1", 8)
			bindInferredBPSSession(c, []byte(tc.body), tc.identity, tc.root, verifiedNewAPIPolicyContext{}, false)
			got, diagnostic := inferredBPSCacheSeed(c.Request.Context(), a, "original-cache", false)
			require.Equal(t, "original-cache", got)
			require.Equal(t, "skipped", diagnostic.Result)
			require.Equal(t, tc.reason, diagnostic.Reason)
		})
	}
	c := inferredSessionFixture(t, inferredOpening, "user-a", "device-a", "client/1", 8)
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeFull} {
		a.CodexFingerprintMode = mode
		got, diagnostic := inferredBPSCacheSeed(c.Request.Context(), a, "original-cache", false)
		require.Equal(t, "original-cache", got)
		require.Nil(t, diagnostic)
	}
	a.CodexFingerprintMode = auth.CodexFingerprintModeSession
	got, diagnostic := inferredBPSCacheSeed(c.Request.Context(), a, "compact-cache", true)
	require.Equal(t, "compact-cache", got)
	require.Equal(t, "compaction_request", diagnostic.Reason)
}

func TestInferredBPSSignedDeviceAndClientSignals(t *testing.T) {
	c := inferredSessionFixture(t, inferredOpening, "user-a", "untrusted-device", "client/1", 8)
	policy := verifiedNewAPIPolicyContext{MetaVerified: true}
	policy.Meta.InstallationID = "signed-device"
	bindInferredBPSSession(c, []byte(inferredOpening), requestSessionIdentity{}, requestRootSessionIdentity{}, policy, true)
	first, _ := c.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	require.Equal(t, "signed_newapi", first.diagnostic.DeviceSource)
	c.Request.Header.Set(codexInstallationIDHeader, "another-untrusted-device")
	bindInferredBPSSession(c, []byte(inferredOpening), requestSessionIdentity{}, requestRootSessionIdentity{}, policy, true)
	next, _ := c.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	require.Equal(t, first.seed, next.seed)
	// Later frames must discard an earlier inference when a real ID arrives.
	bindInferredBPSSession(c, []byte(inferredOpening), requestSessionIdentity{explicitUpstreamID: "real-session"}, requestRootSessionIdentity{}, policy, true)
	next, _ = c.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	require.Empty(t, next.seed)
	require.Equal(t, "explicit_identity", next.diagnostic.Reason)
}

func TestInferredBPSResolverDoesNotCreateOwnership(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	c := transportTestContext()
	c.Set(contextAPIKeyID, int64(8))
	c.Request.Header.Set("User-Agent", "Go-http-client/2.0")
	identity := h.resolveRequestSessionIdentityForContext(c, []byte(inferredOpening))
	state, _ := c.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	require.NotNil(t, state)
	require.NotEmpty(t, state.seed)
	require.False(t, identity.stableIdentity)
	require.False(t, identity.ownsRootBinding)
	require.False(t, identity.hasRequestFingerprint)
	require.Empty(t, identity.explicitUpstreamID)
	require.Empty(t, c.Request.Header.Get("Session-Id"))
	require.Equal(t, "user_conversation_prefix", state.diagnostic.Source)
	// Adding history must retain the inferred seed without changing raw signals.
	identity2 := h.resolveRequestSessionIdentityForContext(c, []byte(inferredFollowup))
	state2, _ := c.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	require.Equal(t, state.seed, state2.seed)
	require.False(t, identity2.stableIdentity)
}

func TestInferredBPSAnchorFormats(t *testing.T) {
	chat := `{"messages":[{"role":"system","content":"keep brief"},{"role":"user","content":"first"}]}`
	responses := `{"input":[{"role":"system","content":"keep brief"},{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"developer","content":"time changes"},{"role":"user","content":"next"}]}`
	require.Equal(t, inferredConversationAnchor([]byte(chat)), inferredConversationAnchor([]byte(responses)))
	require.NotEqual(t, inferredConversationAnchor([]byte(chat)), inferredConversationAnchor([]byte(strings.Replace(chat, "keep brief", "be detailed", 1))))
	require.Equal(t, inferredConversationAnchor([]byte(`{"input":"first"}`)), inferredConversationAnchor([]byte(`{"messages":[{"role":"user","content":"first"}]}`)))
	require.Empty(t, inferredConversationAnchor([]byte(`{"input":[{"role":"user","content":[]}]}`)))
}

func TestBPSInferredSessionWire(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	a := &auth.Account{DBID: 1243, AccountID: "account-a", AccessToken: "test-token", CodexBPS: true, CodexInstallationID: "upstream-device", CodexFingerprintMode: auth.CodexFingerprintModeSession}
	type wire struct{ session, cache, task, turn string }
	var sent []wire
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		sent = append(sent, wire{r.Header.Get("Session-Id"), gjson.GetBytes(body, "prompt_cache_key").String(), gjson.GetBytes(body, "metadata.task_id").String(), gjson.GetBytes(body, "metadata.turn_id").String()})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"test-response","output":[]}`)), Request: r}, nil
	})
	seenProfiles := map[string]bool{}
	for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		a.CodexBPSProfile = profile
		for _, body := range []string{inferredOpening, inferredFollowup} {
			c := inferredSessionFixture(t, body, "user-a", "client-device", "client/1", 8)
			before := string(body)
			ctx, err := WithCodexTestMode(c.Request.Context(), "bps")
			require.NoError(t, err)
			resp, err := ExecuteRequest(ctx, a, []byte(body), NewUpstreamSessionUUID(), "", "test-key", nil, c.Request.Header, false)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			diagnostic := CodexBPSResponseDiagnostic(resp)
			require.Equal(t, "applied", diagnostic.InferredSession.Result)
			require.Equal(t, before, body)
			require.Empty(t, c.Request.Header.Get("Session-Id"), "do not turn an inferred hint into inbound identity")
			require.Equal(t, "applied", snapshotUpstreamTrace(resp.Request.Context()).Transport.BPS.InferredSession.Result)
		}
		left, right := sent[len(sent)-2], sent[len(sent)-1]
		if profile == auth.BPSWord {
			require.Empty(t, left.session)
			require.Empty(t, left.cache)
			require.Empty(t, right.session)
			require.Empty(t, right.cache)
		} else {
			require.NotEmpty(t, left.session)
			require.Equal(t, left.session, left.cache)
			require.Equal(t, left.session, right.session)
		}
		require.Equal(t, left.task, right.task)
		require.NotEqual(t, left.turn, right.turn)
		require.False(t, seenProfiles[left.task])
		seenProfiles[left.task] = true
	}
	// Account separation is applied after the inferred seed.
	seed, _ := inferredBPSCacheSeed(inferredSessionFixture(t, inferredOpening, "user-a", "client-device", "client/1", 8).Request.Context(), a, "unused", false)
	require.NotEqual(t, bpsProfileCacheKey("account-a", seed, bpsProfile(auth.BPSWord)), bpsProfileCacheKey("account-b", seed, bpsProfile(auth.BPSWord)))
}
