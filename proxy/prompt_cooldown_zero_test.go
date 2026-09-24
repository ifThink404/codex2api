package proxy

import (
	"database/sql"
	"testing"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/stretchr/testify/require"
)

func TestZeroUserCooldownTakesEffectOnExistingLocksAndNewIncidents(t *testing.T) {
	h, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	user := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	root := "0123456789abcdef0123456789abcdef"
	first := signedBoundNewAPIPolicyContext(t, "cooldown-zero-first", user, body, 101, "gateway-a", "gateway-a-secret", root)
	setIngressRequestBodyIfAbsent(first, body)
	h.logUpstreamCyberPolicy(first, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	cfg := h.store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.UserCyberCooldownMinutes = 0
	reloaded, err := promptfilter.ParseAdvancedConfig(promptfilter.MarshalAdvancedConfig(cfg.Advanced))
	require.NoError(t, err)
	cfg.Advanced = reloaded
	h.store.SetPromptFilterConfig(cfg)
	require.Zero(t, promptUserCyberCooldownTTL(h.store.GetPromptFilterConfig()))
	for _, fingerprint := range []string{"", "fedcba9876543210fedcba9876543210"} {
		next := signedBoundNewAPIPolicyContext(t, "cooldown-zero-new", user, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
		setIngressRequestBodyIfAbsent(next, body)
		require.False(t, h.inspectPromptFilterOpenAI(next, body, "/v1/responses", "gpt-5.5"), "zero must disable cross-session cooldown immediately")
	}
	same := signedBoundNewAPIPolicyContext(t, "cooldown-zero-same", user, body, 101, "gateway-a", "gateway-a-secret", root)
	setIngressRequestBodyIfAbsent(same, body)
	require.True(t, h.inspectPromptFilterOpenAI(same, body, "/v1/responses", "gpt-5.5"), "separate conversation lock remains enabled")
	otherUser := newAPIIdentity{UserID: "43", ClientIP: "203.0.113.8"}
	noSession := signedBoundNewAPIPolicyContext(t, "cooldown-zero-sessionless", otherUser, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(noSession, body)
	h.logUpstreamCyberPolicy(noSession, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	_, _, err = db.GetActivePromptConversationRestriction(t.Context(), "", "gateway-a", "43", 24*time.Hour, 30*time.Minute)
	require.ErrorIs(t, err, sql.ErrNoRows, "do not create user-only cooldown rows when disabled")
	h.markFingerprintReplayLockCreated()
	require.False(t, h.hasActiveFingerprintReplayLocks(t.Context(), 0), "a cached replay gate must not turn zero into an unlimited lock")
}
