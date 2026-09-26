package auth

import (
	"testing"

	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestRelaxedAccountGroupsSelectionKeepsKeyScope(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed"} {
		t.Run(engine, func(t *testing.T) {
			accounts := sparseRoutingAccounts(16, 9)
			for _, relaxed := range []bool{false, true} {
				store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: engine, CodexForkAccountFallbackEnabled: relaxed})
				t.Cleanup(store.Stop)
				for _, account := range accounts {
					store.AddAccount(account)
				}
				store.SetAPIKeyAllowedGroups(42, []int64{9})
				store.SetAPIKeyNoAffinityGroups(42, []int64{8})
				// Force the indexed engine to cache the current allowed pool.
				first := store.NextExcluding(42, nil)
				require.NotNil(t, first)
				store.Release(first)
				selected := store.NextExcluding(42, map[int64]bool{16: true})
				require.Nil(t, selected, "relaxed mode must remain inside the key's group permissions")
				require.Equal(t, []int64{9}, store.GetAPIKeyAllowedGroups(42))
			}
		})
	}
}

func TestRelaxedAccountGroupsKeepOtherRestrictions(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed"} {
		t.Run(engine, func(t *testing.T) {
			account := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)
			account.GroupIDs, account.PlanType = []int64{30}, "plus"
			store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: engine, CodexForkAccountFallbackEnabled: true})
			t.Cleanup(store.Stop)
			store.AddAccount(account)
			store.SetAPIKeyAllowedGroups(42, []int64{9})
			require.False(t, store.APIKeyAllowsAccount(42, account))
			store.SetAPIKeyAllowedGroups(42, []int64{30})
			require.True(t, store.APIKeyAllowsAccount(42, account))
			store.SetAPIKeyAllowedPlans(42, []string{"team"})
			require.False(t, store.APIKeyAllowsAccount(42, account))
			require.Nil(t, store.NextExcluding(42, nil))
			store.SetAPIKeyAllowedPlans(42, []string{"plus"})
			store.SetAPIKeyUpstreamChannel(42, database.UpstreamChannelGrok)
			require.False(t, store.APIKeyAllowsAccount(42, account))
			require.Nil(t, store.NextExcluding(42, nil))
			store.SetAPIKeyUpstreamChannel(42, database.UpstreamChannelCodex)
			account.SetAllowedAPIKeyIDs([]int64{99})
			require.Nil(t, store.NextExcluding(42, nil), "explicit account/key bindings still apply")
		})
	}
}

func TestRelaxedAccountGroupsSettingsLoadAndReload(t *testing.T) {
	db, err := database.New("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	settings := &database.SystemSettings{MaxConcurrency: 1, CodexForkAccountFallbackEnabled: true}
	store := NewStore(db, nil, settings)
	t.Cleanup(store.Stop)
	account := &Account{DBID: 1, GroupIDs: []int64{30}}
	store.SetAPIKeyAllowedGroups(42, []int64{9})
	require.False(t, store.APIKeyAllowsAccount(42, account), "startup must preserve key group permissions")
	for _, enabled := range []bool{false, true} {
		settings.CodexForkAccountFallbackEnabled = enabled
		require.NoError(t, db.UpdateSystemSettings(t.Context(), settings))
		require.NoError(t, store.reloadSchedulerSettings(t.Context()))
		require.False(t, store.APIKeyAllowsAccount(42, account), "settings reload must preserve key group permissions")
	}
}
