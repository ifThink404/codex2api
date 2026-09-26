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
				account := &auth.Account{DBID: 1, AccessToken: "test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, GroupIDs: []int64{30}}
				h.store.AddAccount(account)
				for _, relaxed := range []bool{false, true, false} {
					UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
					h.store.SetRelaxedAccountGroups(relaxed)
					h.prepareChatGroupRouting(request, identity)
					filter := applyAffinityGroupRouting(request, identity, func(a *auth.Account) bool { return h.store.APIKeyAllowsAccount(7, a) })
					selected := h.store.NextExcludingWithDispatch(7, nil, filter, auth.DispatchPolicyStandard)
					if relaxed {
						require.Same(t, account, selected)
						h.store.Release(selected)
						require.Equal(t, "relaxed_no_groups", usageRequestDiagnosticState(request).GroupRouting.Reason)
					} else {
						require.Nil(t, selected)
					}
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
			target := &auth.Account{DBID: 2, GroupIDs: []int64{30}}
			for _, relaxed := range []bool{false, true, false} {
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				h.prepareChatGroupRouting(request, identity)
				filter := applyAffinityGroupRouting(request, identity, func(*auth.Account) bool { return true })
				require.Equal(t, relaxed, filter(target))
			}
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
