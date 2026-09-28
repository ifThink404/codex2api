package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestQuotaReserveUsesKeyModelAndRouteGates(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	store.SetAPIKeyAllowedGroups(42, []int64{1})
	for _, a := range []*auth.Account{
		{DBID: 1, AccessToken: "synthetic", Status: auth.StatusReady, GroupIDs: []int64{1}, Models: []string{"model-a"}, CodexBPS: true},
		{DBID: 2, AccessToken: "synthetic", Status: auth.StatusReady, GroupIDs: []int64{2}, Models: []string{"model-a"}, CodexBPS: true},
		{DBID: 3, AccessToken: "synthetic", Status: auth.StatusReady, GroupIDs: []int64{1}, Models: []string{"model-b"}, CodexBPS: true},
		{DBID: 4, AccessToken: "synthetic", Status: auth.StatusReady, GroupIDs: []int64{1}, Models: []string{"model-a"}},
	} {
		store.AddAccount(a)
	}
	key := &database.APIKeyRow{ID: 42, Enabled: true, Limits: database.APIKeyLimits{ModelAllow: []string{"model-a"}}}
	allowed, _, reason := QuotaReserveEligibility(store, key, "model-a", "bps")
	require.Empty(t, reason)
	require.Equal(t, []int64{1}, allowed)
	allowed, _, _ = QuotaReserveEligibility(store, key, "model-a", "native")
	require.Equal(t, []int64{4}, allowed)
	allowed, _, reason = QuotaReserveEligibility(store, key, "model-b", "bps")
	require.Empty(t, allowed)
	require.Equal(t, "key_model_not_allowed", reason)
	key.Enabled = false
	allowed, _, reason = QuotaReserveEligibility(store, key, "model-a", "bps")
	require.Empty(t, allowed)
	require.Equal(t, "key_unavailable", reason)
}
