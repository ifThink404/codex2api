package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestRelaxedGroupRoutingInitialSelectionAndHotToggle(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		for _, fingerprint := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/no_fingerprint", true: "/fingerprint"}[fingerprint], func(t *testing.T) {
				h := newWindowAuthorizationHandler(t)
				request, _, identity := chatGroupTestRequest(t, h, path)
				identity.hasRequestFingerprint = fingerprint
				// Select from the other route within the key's configured pool.
				group := int64(20)
				if path == "/v1/chat/completions" || !fingerprint {
					group = 10
				}
				account := &auth.Account{DBID: 1, AccessToken: "test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, GroupIDs: []int64{group}}
				h.store.AddAccount(account)
				outside := &auth.Account{DBID: 2, AccessToken: "test-outside", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, GroupIDs: []int64{30}}
				h.store.AddAccount(outside)
				for _, relaxed := range []bool{false, true, false} {
					UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
					h.prepareChatGroupRouting(request, identity)
					filter := applyAffinityGroupRouting(request, identity, func(a *auth.Account) bool { return h.store.APIKeyAllowsAccount(7, a) })
					selected := h.store.NextExcludingWithDispatch(7, nil, filter, auth.DispatchPolicyStandard)
					if relaxed {
						require.Same(t, account, selected)
						h.store.Release(selected)
						require.Equal(t, "relaxed_key_scope", usageRequestDiagnosticState(request).GroupRouting.Reason)
					} else {
						require.Nil(t, selected)
					}
					require.Nil(t, h.store.NextExcludingWithDispatch(7, map[int64]bool{account.ID(): true}, filter, auth.DispatchPolicyStandard), "exhausting authorized accounts must never widen key scope")
					require.False(t, applyAffinityGroupRouting(request, identity, func(*auth.Account) bool { return false })(account), "inner account eligibility must remain enforced")
				}
			})
		}
	}
}

func TestRelaxedChatGroupRoutingDoesNotPinOldCohort(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "different_group", true: "missing_owner"}[missing], func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			request, _, identity := chatGroupTestRequest(t, h, "/v1/chat/completions")
			owner := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
			if !missing {
				h.store.AddAccount(owner)
			}
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(sessionAffinityKey(identity.affinityID, 7)), database.SessionContinuityRecord{AccountID: 1, ThreadID: identity.affinityID, LastSeen: time.Now()})
			require.NoError(t, err)
			target := &auth.Account{DBID: 2, GroupIDs: []int64{20}}
			outside := &auth.Account{DBID: 3, GroupIDs: []int64{30}}
			for _, relaxed := range []bool{false, true, false} {
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				h.prepareChatGroupRouting(request, identity)
				filter := applyAffinityGroupRouting(request, identity, func(a *auth.Account) bool { return h.store.APIKeyAllowsAccount(7, a) })
				require.Equal(t, relaxed, filter(target))
				require.False(t, filter(outside), "dropping the old cohort must not grant access to another key group")
			}
		})
	}
}

func TestRelaxedFailoverRechecksKeyGroupsBeforeCommit(t *testing.T) {
	for _, scenario := range []string{"membership", "key_permissions"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, true)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			for _, account := range []*auth.Account{owner, target} {
				require.True(t, h.store.ApplyAccountGroups(account.ID(), []int64{1}))
			}
			h.store.SetAPIKeyAllowedGroups(101, []int64{1})
			owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
			require.True(t, h.store.AdmitAccountSession(owner, "occupied", time.Now()))
			request, body := failoverTestRequest(t, h)
			require.Nil(t, h.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			changed := false
			store := &backgroundMatchRaceStore{CodexIdentityStore: h.db, switchOwner: func() {
				if scenario == "membership" {
					require.True(t, h.store.ApplyAccountGroups(target.ID(), []int64{2}))
				} else {
					h.store.SetAPIKeyAllowedGroups(101, []int64{2})
				}
				changed = true
			}}
			selected, _, handled := h.takeSessionAccountFailover(WithCodexIdentityStore(request.Request.Context(), store), key, 101, nil, nil, auth.DispatchPolicyStandard)
			require.True(t, handled)
			require.True(t, changed)
			require.Nil(t, selected)
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.Equal(t, owner.ID(), record.AccountID)
			diagnostic := usageRequestDiagnosticState(request).AccountFailover
			require.Equal(t, "no_safe_candidate", diagnostic.Result)
			require.Positive(t, diagnostic.Selection.RejectionCounts["api_key_scope_mismatch"])
		})
	}
}

func TestRelaxedFailoverAllowsGroupChangesBeforeCommit(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		for _, changeOwner := range []bool{false, true} {
			name := map[bool]string{false: "strict", true: "relaxed"}[relaxed] + "/" + map[bool]string{false: "target", true: "owner"}[changeOwner]
			t.Run(name, func(t *testing.T) {
				h, owner, target, key := failoverTestSetup(t, true)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
				require.True(t, h.store.AdmitAccountSession(owner, "occupied", time.Now()))
				request, body := failoverTestRequest(t, h)
				require.Nil(t, h.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				changed := false
				store := &backgroundMatchRaceStore{CodexIdentityStore: h.db, switchOwner: func() {
					account := target
					if changeOwner {
						account = owner
					}
					require.True(t, h.store.ApplyAccountGroups(account.ID(), []int64{99}))
					changed = true
				}}
				selected, _, handled := h.takeSessionAccountFailover(WithCodexIdentityStore(request.Request.Context(), store), key, 0, nil, nil, auth.DispatchPolicyStandard)
				require.True(t, handled)
				require.True(t, changed)
				record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				if relaxed {
					require.Same(t, target, selected)
					h.store.Release(selected)
					require.Equal(t, target.ID(), record.AccountID)
				} else {
					require.Nil(t, selected)
					require.Equal(t, owner.ID(), record.AccountID)
					require.Equal(t, "account_groups_changed", usageRequestDiagnosticState(request).AccountFailover.Reason)
				}
			})
		}
	}
}
