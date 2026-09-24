package auth

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBPSProfileDefaultsAndPeerSnapshot(t *testing.T) {
	a := &Account{}
	require.Equal(t, BPSWord, a.EffectiveCodexBPSProfile())
	s := &Store{}
	for _, profile := range []CodexBPSProfile{BPSExcel, BPSSheets, BPSPowerPoint, BPSWord} {
		s.applyPersistentAccountSnapshot(a, &Account{CodexBPSProfile: profile}, true)
		require.Equal(t, profile, a.EffectiveCodexBPSProfile())
	}
	var p CodexBPSProfile
	for _, bad := range []string{`null`, `""`, `"other"`, `["word","excel"]`, `true`} {
		require.Error(t, json.Unmarshal([]byte(bad), &p))
	}
}
