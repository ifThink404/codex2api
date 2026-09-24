package database

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestZeroCooldownIgnoresClockSkewAndDoesNotShowUserRestriction(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	key := strings.Repeat("a", 64)
	_, _, err := db.LockPromptConversation(t.Context(), PromptConversationLockInput{
		LockKey: key, Platform: "newapi", NewAPIUserID: "42", SessionHash: "original-session",
		SessionFingerprint: strings.Repeat("a", 32), DecisionID: "future-cyb", ReasonCode: "upstream_cyber_policy",
		LockedAt: time.Now().Add(time.Minute),
	})
	require.NoError(t, err)
	_, _, err = db.GetActivePromptConversationRestriction(t.Context(), "", "newapi", "42", time.Hour, 0)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, exact, err := db.GetActivePromptConversationRestriction(t.Context(), key, "newapi", "42", time.Hour, 0)
	require.NoError(t, err)
	require.True(t, exact)
	_, _, err = db.LockPromptConversation(t.Context(), PromptConversationLockInput{
		LockKey: strings.Repeat("b", 64), IdentityKind: PromptConversationLockIdentityFingerprintReplay,
		Platform: "codex-fingerprint", NewAPIUserID: "apikey:1", SessionHash: "replay-session", SessionFingerprint: strings.Repeat("b", 32),
		DecisionID: "replay-cyb", ReasonCode: "upstream_cyber_policy", LockedAt: time.Now(),
	})
	require.NoError(t, err)
	replay, err := db.HasActivePromptFingerprintReplayLocks(t.Context(), 0)
	require.NoError(t, err)
	require.False(t, replay)
	active, err := db.promptRiskActiveRestrictionSubjects(t.Context(), time.Hour, 0)
	require.NoError(t, err)
	require.Contains(t, active, PromptRiskSubjectSession+"\x00original-session")
	require.NotContains(t, active, PromptRiskSubjectSession+"\x00replay-session")
	for _, restriction := range active {
		require.NotEqual(t, PromptRiskSubjectNewAPIUser, restriction.Profile.SubjectType)
	}
}
