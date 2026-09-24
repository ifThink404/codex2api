package promptfilter

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUserCyberCooldownZeroSurvivesConfigMergeAndReload(t *testing.T) {
	for _, raw := range []string{`{}`, `{"enforcement":{}}`} {
		cfg, err := ParseAdvancedConfig(raw)
		require.NoError(t, err)
		require.Equal(t, 30, cfg.Enforcement.UserCyberCooldownMinutes)
	}
	doc, err := MergeAdvancedConfigDocument(`{}`, `{"enforcement":{"user_cyber_cooldown_minutes":0}}`)
	require.NoError(t, err)
	require.Zero(t, doc.Effective.Enforcement.UserCyberCooldownMinutes)
	for _, raw := range []string{doc.Raw, MarshalAdvancedConfig(doc.Effective)} {
		cfg, err := ParseAdvancedConfig(raw)
		require.NoError(t, err)
		require.Zero(t, cfg.Enforcement.UserCyberCooldownMinutes)
	}
	negative, err := ParseAdvancedConfig(`{"enforcement":{"user_cyber_cooldown_minutes":-1}}`)
	require.NoError(t, err)
	require.Equal(t, 30, negative.Enforcement.UserCyberCooldownMinutes)
}
