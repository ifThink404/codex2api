package proxy

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRelaxedRootFailoverWithKnownBPSCompaction(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		for _, reason := range []string{"disabled", "paused", "quota_5h", "quota_7d", "capacity", "payment_required"} {
			t.Run(fmt.Sprintf("%s/preserve_%v", reason, preserve), func(t *testing.T) {
				h, owner, target, key := failoverTestSetup(t, false)
				t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
					s.CodexForkAccountFallbackEnabled, s.CodexSessionFailoverPreserveInput = true, preserve
					return s
				})
				h.store.SetLazyMode(true)
				off := false
				owner.CodexNative, owner.CodexBPS = &off, true
				target.CodexNative, target.CodexBPS = &off, true
				record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				record.UpstreamMode = "bps"
				key += "-bps"
				h.store.BindSessionAffinity(key, owner, "")
				_, err = h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), record)
				require.NoError(t, err)
				switch reason {
				case "disabled":
					atomic.StoreInt32(&owner.Disabled, 1)
				case "paused":
					atomic.StoreInt32(&owner.DispatchPaused, 1)
				case "payment_required":
					owner.Status, owner.CooldownReason, owner.CooldownUtil = auth.StatusCooldown, "payment_required", time.Now().Add(time.Hour)
				case "quota_5h":
					owner.PlanType = "pro"
					owner.UsagePercent5h, owner.UsagePercent5hValid, owner.Reset5hAt = 100, true, time.Now().Add(time.Hour)
				case "quota_7d":
					owner.UsagePercent7d, owner.UsagePercent7dValid, owner.Reset7dAt = 100, true, time.Now().Add(time.Hour)
				case "capacity":
					owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
					require.True(t, h.store.AdmitAccountSession(owner, "occupied", time.Now()))
				}
				c, body := failoverTestRequest(t, h)
				body, err = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"old-bps-state"},{"role":"user","content":"continue"}]`))
				require.NoError(t, err)
				body = addSessionTools(t, body)
				c.Set(ingressRequestBodyContextKey, body)
				require.NoError(t, h.recordCompactionProvenance(c.Request.Context(), owner, "old-bps-state"))
				origin, err := h.resolveCompactionAffinity(c.Request.Context(), body)
				require.NoError(t, err)
				require.True(t, origin.Known)
				require.Equal(t, bpsCodexCompactionDomain, origin.CompatibilityDomain)
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				routingBody, _ := sessionRestartRoutingContext(c, body)
				affinity, err := h.resolveCompactionAffinity(c.Request.Context(), routingBody)
				require.NoError(t, err)
				filter := sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false)
				if affinity.Known {
					applyCompactionRouteFloor(c, affinity)
					filter = compactionDomainFilter(affinity.CompatibilityDomain, filter, selectionTraceForRequest(c))
				}
				selected, _, _ := h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), key, 0, newRetryAccountExclusions(), filter, auth.DispatchPolicyStandard)
				if selected != target {
					diagnostic, _ := json.Marshal(usageRequestDiagnosticState(c).AccountFailover)
					t.Fatalf("selected target=%v, failover=%s", selected == target, diagnostic)
				}
				h.store.Release(selected)
				require.Equal(t, target.ID(), selectionTraceForRequest(c).PinnedAccount())
				diagnostic := usageRequestDiagnosticState(c).AccountFailover
				require.Equal(t, "switched", diagnostic.Result)
				require.Equal(t, "relaxed_mode", diagnostic.EnabledBy)
				cleaned, _, err := PrepareSessionRestartOutbound(c.Request.Context(), target, body, c.Request.Header)
				require.NoError(t, err)
				assertSessionTools(t, cleaned)
				if preserve {
					require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
				} else {
					require.NotContains(t, string(cleaned), "old-bps-state")
				}
				stored, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, target.ID(), stored.AccountID)
				require.Equal(t, "bps", stored.UpstreamMode)
				require.Equal(t, preserve, stored.PreserveRestartInput)
				require.EqualValues(t, 1, stored.FailoverCount)
				h.continuityRecords = nil
				resumed, _ := failoverTestRequest(t, h)
				require.Nil(t, h.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				require.Equal(t, target.ID(), selectionTraceForRequest(resumed).PinnedAccount())
			})
		}
	}
}

func TestRelaxedRootFailoverKeepsCandidateRestrictions(t *testing.T) {
	for _, scenario := range []string{"groups", "model", "native_only", "no_capacity"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			atomic.StoreInt32(&owner.Disabled, 1)
			h.store.SetSchedulerEngine("legacy")
			target.CodexBPS = true
			switch scenario {
			case "groups":
				require.True(t, h.store.ApplyAccountGroups(owner.ID(), []int64{11}))
				require.True(t, h.store.ApplyAccountGroups(target.ID(), []int64{12}))
			case "model":
				target.Models = []string{"gpt-6-astra"}
			case "native_only":
				target.CodexBPS = false
			case "no_capacity":
				target.SessionCapacityEnabled, target.SessionCapacityMax = true, 1
				require.True(t, h.store.AdmitAccountSession(target, "occupied", time.Now()))
			}
			c, body := failoverTestRequest(t, h)
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			applyCompactionRouteFloor(c, compactionAffinityResolution{Known: true, CompatibilityDomain: bpsCodexCompactionDomain, PreferredAccountID: owner.ID()})
			filter := compactionDomainFilter(bpsCodexCompactionDomain, sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false), selectionTraceForRequest(c))
			selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, filter, auth.DispatchPolicyStandard)
			require.True(t, handled)
			if scenario == "groups" {
				require.Same(t, target, selected)
				h.store.Release(selected)
				record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.Equal(t, target.ID(), record.AccountID)
				require.Equal(t, "relaxed_no_groups", usageRequestDiagnosticState(c).AccountFailover.Selection.MatchMode)
				return
			}
			require.Nil(t, selected)
			diagnostic := usageRequestDiagnosticState(c).AccountFailover
			require.Equal(t, "no_safe_candidate", diagnostic.Result)
			require.NotEmpty(t, diagnostic.Selection.RejectionCounts)
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.Equal(t, owner.ID(), record.AccountID)
		})
	}
}

func TestRootFailoverDisabledIsDiagnosed(t *testing.T) {
	h, owner, _, key := failoverTestSetup(t, false)
	atomic.StoreInt32(&owner.Disabled, 1)
	c, body := failoverTestRequest(t, h)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	diagnostic := usageRequestDiagnosticState(c).AccountFailover
	require.Equal(t, "disabled", diagnostic.Result)
	require.Equal(t, "account_disabled", diagnostic.TriggerReason)
	require.Equal(t, "failover_disabled", diagnostic.BlockReason)
	raw, err := json.Marshal(diagnostic)
	require.NoError(t, err)
	require.Equal(t, owner.ID(), gjson.GetBytes(raw, "previous_account_id").Int())
	selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
	require.False(t, handled)
	require.Nil(t, selected)
}

func TestRelaxedRootFailoverIngress(t *testing.T) {
	t.Run("responses", func(t *testing.T) { runSessionAccountFailoverIngress(t, false, false, false, true) })
	t.Run("compact", func(t *testing.T) { runSessionAccountFailoverIngress(t, true, false, false, true) })
}

func TestRelaxedRootFailoverAfterObservedQuota(t *testing.T) {
	h, owner, target, key := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	c, body := failoverTestRequest(t, h)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	exclusions := newSessionRetryAccountExclusions(c, key, body)
	owner.UsagePercent7d, owner.UsagePercent7dValid, owner.Reset7dAt = 100, true, time.Now().Add(time.Hour)
	exclusions.MarkHTTPFailure(owner.ID(), 429, []byte(`{"error":{"type":"usage_limit_reached"}}`), 0, 1)
	ctx, blocked := h.prepareSessionQuotaRetry(c.Request.Context(), key, exclusions, auth.DispatchPolicyStandard)
	require.False(t, blocked)
	selected, _, handled := h.takeSessionAccountFailover(ctx, key, 0, exclusions.ForSelection(), nil, auth.DispatchPolicyStandard)
	require.True(t, handled)
	require.Same(t, target, selected)
	h.store.Release(selected)
}
