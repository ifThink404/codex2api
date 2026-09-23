package auth

import (
	"testing"

	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestCodexRouteSwitchesAndModels(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, bps := range []bool{false, true} {
			a := &Account{CodexNative: &native, CodexBPS: bps, CodexNativeModels: []string{"gpt-5.6-*"}, CodexBPSModels: []string{"gpt-6-astra"}}
			require.Equal(t, native, a.CodexRouteAllows("native", "gpt-5.6-sol", false))
			require.False(t, a.CodexRouteAllows("native", "gpt-6-astra", false))
			require.Equal(t, bps, a.CodexRouteAllows("bps", "GPT-6-ASTRA", false))
			require.False(t, a.CodexRouteAllows("bps", "gpt-5.6-sol", false))
			require.Equal(t, bps, a.CodexRouteAllows("bps", "codex-auto-review", true))
		}
	}
	for _, bps := range []bool{false, true} {
		a := &Account{CodexBPS: bps}
		require.Equal(t, !bps, a.CodexRouteAllows("native", "any-model", false))
		require.Equal(t, bps, a.CodexRouteAllows("bps", "any-model", false))
	}
}

func TestCodexRouteModelValidation(t *testing.T) {
	require.NoError(t, ValidateCodexRouteModels([]string{"gpt-5.6-*", "gpt-6-astra", "*"}))
	for _, value := range []string{"", "gpt*6", "a/b", "a b", "?"} {
		require.Error(t, ValidateCodexRouteModels([]string{value}))
	}
}

func TestCodexRouteUpdateKeepsOtherSwitch(t *testing.T) {
	for _, old := range []bool{false, true} {
		store := NewStore(nil, nil, nil)
		a := &Account{DBID: 1, CodexBPS: old}
		store.AddAccount(a)
		store.ApplyAccountCodexRoutes(1, database.OptionalBool{}, database.OptionalBool{Set: true, Value: !old}, nil, nil, false, false)
		require.Equal(t, !old, a.CodexRouteAllows("native", "", true))
		require.Equal(t, !old, a.CodexRouteAllows("bps", "", true))
		store.Stop()
	}
}
