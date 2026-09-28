package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestSessionRetryFailoverSharedWorkspace(t *testing.T) {
	for _, mode := range []string{"native", "bps"} {
		for _, tc := range []struct {
			name, rejection string
			relaxed         bool
		}{
			{name: "strict_shared", rejection: "account_identity_ineligible"},
			{name: "strict_distinct"},
			{name: "relaxed_shared", relaxed: true},
			{name: "relaxed_header_override", relaxed: true},
			{name: "relaxed_missing_workspace", relaxed: true, rejection: "account_identity_ineligible"},
			{name: "relaxed_key_scope", relaxed: true, rejection: "api_key_scope_mismatch"},
			{name: "relaxed_capacity", relaxed: true, rejection: "session_capacity_exhausted"},
			{name: "relaxed_target_excluded", relaxed: true, rejection: "request_excluded"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				// Exercise relaxed mode on its own, and retain strict failover as
				// the control. Distinct credentials can share one workspace.
				h, owner, target, key := failoverTestSetup(t, !tc.relaxed)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
					s.CodexForkAccountFallbackEnabled = tc.relaxed
					return s
				})
				native := mode == "native"
				owner.CodexNative, owner.CodexBPS = &native, !native
				target.CodexNative, target.CodexBPS = &native, !native
				if tc.name == "relaxed_header_override" {
					target.CustomHeaders = map[string]string{"Chatgpt-Account-Id": owner.AccountID}
				} else if tc.name != "strict_distinct" {
					target.AccountID = owner.AccountID
				}
				switch tc.name {
				case "relaxed_missing_workspace":
					target.AccountID = ""
				case "relaxed_key_scope":
					require.True(t, h.store.ApplyAccountGroups(owner.ID(), []int64{1}))
					require.True(t, h.store.ApplyAccountGroups(target.ID(), []int64{2}))
					h.store.SetAPIKeyAllowedGroups(101, []int64{1})
				case "relaxed_capacity":
					target.SessionCapacityEnabled, target.SessionCapacityMax = true, 1
					require.True(t, h.store.AdmitAccountSession(target, "occupied", time.Now()))
				}
				record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				record.UpstreamMode = mode
				key += "-shared-workspace-" + mode
				h.store.BindSessionAffinity(key, owner, "")
				_, err = h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), record)
				require.NoError(t, err)
				request, body := failoverTestRequest(t, h)
				body = addSessionTools(t, body)
				request.Set(ingressRequestBodyContextKey, body)
				require.Nil(t, h.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				exclusions := newSessionRetryAccountExclusions(request, key, body)
				exclusions.MarkHTTPFailure(owner.ID(), 500, []byte(`{"error":{"message":"temporary"}}`), 1, 1)
				if tc.name == "relaxed_target_excluded" {
					exclusions.MarkHard(target.ID())
				}
				ctx, blocked := h.prepareSessionRetryFailover(request.Request.Context(), key, exclusions, auth.DispatchPolicyStandard)
				require.False(t, blocked)
				selected, _, handled := h.takeSessionAccountFailover(ctx, key, 101, exclusions.ForSelection(), nil, auth.DispatchPolicyStandard)
				require.True(t, handled)
				stored, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.True(t, found)
				diagnostic := usageRequestDiagnosticState(request).AccountFailover
				require.Equal(t, "request_excluded", diagnostic.TriggerReason)
				if tc.rejection != "" {
					require.Nil(t, selected)
					require.Equal(t, "no_safe_candidate", diagnostic.Result)
					require.Positive(t, diagnostic.Selection.RejectionCounts[tc.rejection])
					require.Equal(t, owner.ID(), stored.AccountID)
					require.Zero(t, stored.FailoverCount)
					return
				}
				require.Same(t, target, selected)
				defer h.store.Release(selected)
				require.Equal(t, "switched", diagnostic.Result)
				require.Equal(t, target.ID(), stored.AccountID)
				require.Equal(t, owner.ID(), stored.PreviousAccountID)
				require.EqualValues(t, 1, stored.FailoverCount)
				require.Equal(t, mode, stored.UpstreamMode)
				require.True(t, stored.OutboundWindowReset)
				require.NoError(t, validateSessionOutboundEpoch(request.Request.Context(), target))
				require.Error(t, validateSessionOutboundEpoch(request.Request.Context(), owner))
				cleaned, _, err := PrepareSessionRestartOutbound(request.Request.Context(), target, body, request.Request.Header)
				require.NoError(t, err)
				assertSessionTools(t, cleaned)
				h.continuityRecords = nil
				resumed, _ := failoverTestRequest(t, h)
				require.Nil(t, h.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				require.Equal(t, target.ID(), selectionTraceForRequest(resumed).PinnedAccount())
			})
		}
	}
}
