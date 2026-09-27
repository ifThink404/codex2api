package proxy

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func dispatchRecoveryLog(t *testing.T, fields map[string]any) {
	t.Helper()
	data, err := json.Marshal(fields)
	require.NoError(t, err)
	t.Log("DISPATCH_RECOVERY " + string(data))
}

func TestDispatchRecoveryKeyPermissionChangedDuringFailover(t *testing.T) {
	for _, scenario := range []string{"key_groups", "account_key_allowlist"} {
		t.Run(scenario, func(t *testing.T) { dispatchKeyChangeDuringFailover(t, scenario) })
	}
}

func dispatchKeyChangeDuringFailover(t *testing.T, scenario string) {
	h, owner, target, key := failoverTestSetup(t, true)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	for _, a := range []*auth.Account{owner, target} {
		require.True(t, h.store.ApplyAccountGroups(a.ID(), []int64{1}))
	}
	h.store.SetAPIKeyAllowedGroups(101, []int64{1})
	atomic.StoreInt32(&owner.Disabled, 1)
	c, body := failoverTestRequest(t, h)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	changed := false
	store := &backgroundMatchRaceStore{CodexIdentityStore: h.db, switchOwner: func() {
		if scenario == "key_groups" {
			h.store.SetAPIKeyAllowedGroups(101, []int64{2})
		} else {
			target.SetAllowedAPIKeyIDs([]int64{999})
		}
		changed = true
	}}
	selected, _, handled := h.takeSessionAccountFailover(WithCodexIdentityStore(c.Request.Context(), store), key, 101, nil, nil, auth.DispatchPolicyStandard)
	require.True(t, changed)
	require.True(t, handled)
	selectedID := int64(0)
	if selected != nil {
		selectedID = selected.ID()
		h.store.Release(selected)
	}
	record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	d := usageRequestDiagnosticState(c).AccountFailover
	dispatchRecoveryLog(t, map[string]any{"scenario": "permission_revoked/" + scenario, "selected": selectedID, "durable_owner": record.AccountID, "original_owner": owner.ID(), "target_allowed_now": h.store.APIKeyAllowsAccount(101, target) && target.AllowsAPIKey(101), "result": d.Result, "rejections": d.Selection.RejectionCounts})
	require.Nil(t, selected, "revoked Key authorization must be rechecked before committing the replacement")
	require.Equal(t, owner.ID(), record.AccountID)
}
