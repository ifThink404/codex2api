package proxy

import (
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRelaxedMissingOwnerHTTPIngress(t *testing.T) {
	// Ordinary HTTP request, lossy replay, relaxed mode, metadata present,
	// then remove the original account between the two requests.
	runSessionAccountFailoverIngress(t, false, false, false, true, false, true)
}

func missingOwnerSetup(t *testing.T, state string) (*Handler, int64, *auth.Account, string, *gin.Context, []byte) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	settings.CodexForkAccountFallbackEnabled = true
	ApplyRuntimeSettings(settings)
	h := newWindowAuthorizationHandler(t)
	h.store = auth.NewStore(h.db, nil, &database.SystemSettings{MaxConcurrency: 5, CodexForkAccountFallbackEnabled: true})
	t.Cleanup(h.store.Stop)
	config := h.store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = "off"
	h.store.SetPromptFilterConfig(config)
	ownerID := int64(1695)
	groupID, err := h.db.CreateAccountGroup(t.Context(), "original", "", "", 0, 0, sql.NullInt64{})
	require.NoError(t, err)
	if state != "not_found" {
		ownerID, err = h.db.InsertATAccount(t.Context(), "original", "owner-test-token", "")
		require.NoError(t, err)
		require.NoError(t, h.db.UpdateCredentials(t.Context(), ownerID, map[string]interface{}{"account_id": accountIdentitySampleAccount, "models": []string{"gpt-5.6-sol"}, auth.CodexBPSEnabledCredentialKey: true}))
		require.NoError(t, h.db.SetAccountGroups(t.Context(), ownerID, []int64{groupID}))
		if state == "deleted" {
			require.NoError(t, h.db.SoftDeleteAccount(t.Context(), ownerID))
		}
		if state == "credentials_missing" {
			require.NoError(t, h.db.UpdateCredentials(t.Context(), ownerID, map[string]interface{}{"access_token": ""}))
		}
	}
	target := &auth.Account{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "target-test-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexBPS: true, GroupIDs: []int64{groupID}}
	h.store.AddAccount(target)
	key := "deleted-root::api-key:101"
	_, err = h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: ownerID, ThreadID: continuityTestThread, Number: 6, NumberKnown: true, UpstreamMode: "bps", LastSeen: time.Now()})
	require.NoError(t, err)
	c, body := failoverTestRequest(t, h)
	body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.window_id", continuityTestThread+":6")
	require.NoError(t, err)
	body = addSessionTools(t, body)
	c.Set(ingressRequestBodyContextKey, body)
	return h, ownerID, target, key, c, body
}

func TestRelaxedMissingOwnerSwitchesAndPersists(t *testing.T) {
	for _, state := range []string{"not_found", "deleted", "credentials_missing"} {
		for _, preserve := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/preserve_%v", state, preserve), func(t *testing.T) {
				h, ownerID, target, key, c, body := missingOwnerSetup(t, state)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionFailoverPreserveInput = preserve; return s })
				body, err := sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"compaction","encrypted_content":"old-account-state"}`))
				require.NoError(t, err)
				c.Set(ingressRequestBodyContextKey, body)
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				diagnostic := usageRequestDiagnosticState(c).AccountFailover
				require.Equal(t, "account_missing", diagnostic.TriggerReason)
				require.Equal(t, state, diagnostic.OwnerLookup)
				selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 101, nil, sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false), auth.DispatchPolicyStandard)
				require.True(t, handled)
				require.Same(t, target, selected, "%+v", diagnostic)
				h.store.Release(selected)
				require.Nil(t, h.commitSessionContinuity(c, target))
				record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, target.ID(), record.AccountID)
				require.Equal(t, ownerID, record.PreviousAccountID)
				require.EqualValues(t, 1, record.FailoverCount)
				require.Equal(t, "bps", record.UpstreamMode)
				require.Equal(t, preserve, record.PreserveRestartInput)
				require.Equal(t, "relaxed_key_scope", diagnostic.Selection.MatchMode)
				require.Empty(t, diagnostic.Selection.RequiredGroupIDs)
				require.Nil(t, h.store.FindByID(ownerID), "deleted owner must never be reactivated")
				cleaned, _, err := PrepareSessionRestartOutbound(c.Request.Context(), target, body, c.Request.Header)
				require.NoError(t, err)
				assertSessionTools(t, cleaned)
				if preserve {
					require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
				} else {
					require.NotContains(t, string(cleaned), "old-account-state")
				}
				h.continuityRecords = nil
				h.store.UnbindSessionAffinity(key, target.ID())
				resumed, _ := failoverTestRequest(t, h)
				require.Nil(t, h.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				require.Equal(t, target.ID(), selectionTraceForRequest(resumed).PinnedAccount())
			})
		}
	}
}

func TestRelaxedMissingOwnerReloadsExistingAccount(t *testing.T) {
	h, ownerID, _, key, c, body := missingOwnerSetup(t, "not_loaded")
	require.Nil(t, h.store.FindByID(ownerID))
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	require.NotNil(t, h.store.FindByID(ownerID))
	require.Equal(t, ownerID, selectionTraceForRequest(c).PinnedAccount())
	require.Equal(t, "owner_reloaded", usageRequestDiagnosticState(c).AccountFailover.Result)
	record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.Equal(t, ownerID, record.AccountID)
	require.Zero(t, record.FailoverCount)
}

func TestRelaxedMissingOwnerKeepsCandidateGuards(t *testing.T) {
	for _, scenario := range []string{"groups", "api_key", "model", "route", "cooldown", "capacity", "scope_group"} {
		t.Run(scenario, func(t *testing.T) {
			state := "deleted"
			if scenario == "scope_group" {
				state = "not_found"
			}
			h, ownerID, target, key, c, body := missingOwnerSetup(t, state)
			switch scenario {
			case "groups":
				h.store.ApplyAccountGroups(target.ID(), []int64{999})
			case "api_key":
				target.SetAllowedAPIKeyIDs([]int64{999})
			case "model":
				target.Models = []string{"another-model"}
			case "route":
				target.CodexBPS = false
				off := false
				target.CodexNative = &off
			case "cooldown":
				target.Status, target.CooldownReason, target.CooldownUtil = auth.StatusCooldown, "payment_required", time.Now().Add(time.Hour)
			case "capacity":
				target.SessionCapacityEnabled, target.SessionCapacityMax = true, 1
				require.True(t, h.store.AdmitAccountSession(target, "occupied", time.Now()))
			case "scope_group":
				h.store.SetAPIKeyAllowedGroups(101, []int64{999})
			}
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 101, nil, sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false), auth.DispatchPolicyStandard)
			require.True(t, handled)
			if scenario == "groups" {
				require.Same(t, target, selected)
				h.store.Release(selected)
				record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.Equal(t, target.ID(), record.AccountID)
				require.EqualValues(t, 1, record.FailoverCount)
				return
			}
			require.Nil(t, selected)
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.Equal(t, ownerID, record.AccountID)
			require.Zero(t, record.FailoverCount)
		})
	}
}

func TestRelaxedMissingOwnerDisabledKeepsBinding(t *testing.T) {
	h, ownerID, _, key, c, body := missingOwnerSetup(t, "deleted")
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = false; return s })
	failure := h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
	require.NotNil(t, failure)
	require.Equal(t, "codex_sticky_account_unavailable", string(failure.Code))
	record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.Equal(t, ownerID, record.AccountID)
}

func TestRelaxedMissingOwnerLookupFailureKeepsBinding(t *testing.T) {
	h, ownerID, _, key, c, body := missingOwnerSetup(t, "deleted")
	require.Nil(t, h.prepareSessionContinuity(c, requestSessionIdentity{stableIdentity: true}, key, body))
	originalDB := h.db
	failedDB, err := database.New("sqlite", ":memory:")
	require.NoError(t, err)
	require.NoError(t, failedDB.Close())
	h.db = failedDB
	pending, failure := h.prepareSessionAccountFailover(c, key, body, auth.DispatchPolicyStandard)
	require.False(t, pending)
	require.NotNil(t, failure)
	require.Equal(t, "lookup_failed", usageRequestDiagnosticState(c).AccountFailover.OwnerLookup)
	require.Equal(t, "ownership_unavailable", usageRequestDiagnosticState(c).AccountFailover.BlockReason)
	record, _, err := originalDB.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.Equal(t, ownerID, record.AccountID)
	require.Zero(t, record.FailoverCount)
}

func TestRelaxedMissingOwnerConcurrentSwitchAndRestoration(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint(restore), func(t *testing.T) {
			h, ownerID, target, key, first, body := missingOwnerSetup(t, "deleted")
			second, _ := failoverTestRequest(t, h)
			for _, c := range []*gin.Context{first, second} {
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			}
			if restore {
				require.NoError(t, h.db.RestoreAccount(t.Context(), ownerID))
			}
			var winners atomic.Int32
			var wait sync.WaitGroup
			for _, c := range []*gin.Context{first, second} {
				wait.Add(1)
				go func() {
					defer wait.Done()
					selected, _, _ := h.takeSessionAccountFailover(c.Request.Context(), key, 101, nil, nil, auth.DispatchPolicyStandard)
					if selected != nil {
						winners.Add(1)
						h.store.Release(selected)
					}
				}()
			}
			wait.Wait()
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			if restore {
				require.Zero(t, winners.Load())
				require.Equal(t, ownerID, record.AccountID)
			} else {
				require.EqualValues(t, 1, winners.Load())
				require.Equal(t, target.ID(), record.AccountID)
				require.EqualValues(t, 1, record.FailoverCount)
			}
		})
	}
}
