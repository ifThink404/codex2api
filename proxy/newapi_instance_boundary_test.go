package proxy

import (
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/stretchr/testify/require"
)

// A signing secret authenticates a configured binding; it is not a source
// instance ID. Sharing one API key/platform gives same-numbered users one scope.
func TestNewAPISharedSecretInstanceBoundary(t *testing.T) {
	const secret = "same-audit-secret-for-test"
	h := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{
		{APIKeyID: 101, PlatformCode: "gateway-a", Secret: secret, Enabled: true},
		{APIKeyID: 202, PlatformCode: "gateway-b", Secret: secret, Enabled: true},
	})
	body := []byte(`{"model":"gpt-5.6-sol","input":"test"}`)
	fingerprint := promptSessionTestFingerprint("same-client-session")
	verify := func(key int64, platform, request, ip string, channel int) (verifiedNewAPIPolicyContext, string, string, string, bool) {
		c := signedBoundNewAPIPolicyContext(t, request, newAPIIdentity{UserID: "42", ClientIP: ip}, body, key, platform, secret, fingerprint)
		addSignedNewAPIPolicyMetaWithSecret(t, c, newAPIPolicyMeta{
			PlatformID: platform, TokenID: 8, ChannelID: channel,
			Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
			Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses), SessionFingerprint: fingerprint,
		}, true, secret)
		cfg := h.promptFilterConfigForRequest(c)
		policy, ok := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, body)
		bindTransportOwner(c, policy, ok)
		return policy, verifiedTransportUser(c.Request.Context()), WebsocketTransportOwner(c.Request.Context(), "same-downstream-key"), responseCacheOwnerForRequest(c, key), ok
	}
	a, userA, transportA, cacheA, ok := verify(101, "gateway-a", "request-a", "203.0.113.1", 1)
	require.True(t, ok)
	b, userB, transportB, cacheB, ok := verify(101, "gateway-a", "request-b", "203.0.113.2", 1)
	require.True(t, ok)
	require.Equal(t, newAPIRuntimeScopeForPolicyContext(a), newAPIRuntimeScopeForPolicyContext(b))
	require.NotEmpty(t, userA)
	require.Equal(t, userA, userB, "different NewAPI hosts are not an identity boundary")
	require.Equal(t, transportA, transportB)
	require.Equal(t, cacheA, cacheB)
	channel, userChannel, transportChannel, cacheChannel, ok := verify(101, "gateway-a", "request-channel", "203.0.113.2", 2)
	require.True(t, ok)
	require.NotEqual(t, newAPIRuntimeScopeForPolicyContext(a), newAPIRuntimeScopeForPolicyContext(channel))
	require.NotEqual(t, transportA, transportChannel)
	require.Equal(t, userA, userChannel, "channel IDs intentionally do not separate person identity")
	require.Equal(t, cacheA, cacheChannel)
	_, userOther, transportOther, cacheOther, ok := verify(202, "gateway-b", "request-a", "203.0.113.1", 1)
	require.True(t, ok, "another binding accepts identical request/user/channel IDs")
	require.NotEqual(t, userA, userOther)
	require.NotEqual(t, transportA, transportOther)
	require.NotEqual(t, cacheA, cacheOther)
	_, _, _, _, ok = verify(101, "gateway-a", "request-a", "203.0.113.2", 1)
	require.False(t, ok, "same binding also shares request-ID replay protection")
	_, _, _, _, ok = verify(101, "gateway-b", "wrong-platform", "203.0.113.2", 1)
	require.False(t, ok, "a key cannot claim a second configured platform")
}
