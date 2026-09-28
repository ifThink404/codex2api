package proxy

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRelaxedFailoverHeaderOnlyHTTPIngress(t *testing.T) {
	runSessionAccountFailoverIngress(t, false, false, false, true, true)
}

func TestRelaxedFailoverLegacyWindow(t *testing.T) {
	for _, persisted := range []bool{true, false} {
		for _, mode := range []string{"off", "observe", "enforce"} {
			for _, path := range []string{"/v1/responses", "/v1/responses/compact", "websocket"} {
				t.Run(fmt.Sprintf("persisted_%v/%s/%s", persisted, mode, path), func(t *testing.T) {
					websocket := path == "websocket"
					h, owner, target, key, c, body := relaxedOwnerRecoverySetup(t, persisted, true, mode)
					if websocket {
						c.Request.Header.Set("Connection", "Upgrade")
						c.Request.Header.Set("Upgrade", "websocket")
						c.Request.Header.Set(codexWindowIDHeader, "bad-handshake-window")
						body, _ = sjson.SetBytes(body, "client_metadata.x-codex-window-id", continuityTestThread)
					} else {
						c.Request.Header.Set(codexWindowIDHeader, continuityTestThread)
						c.Request.URL.Path = path
					}
					c.Set(ingressRequestBodyContextKey, body)
					failure := h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
					if mode == "enforce" {
						require.NotNil(t, failure)
						return
					}
					require.Nil(t, failure)
					d := usageRequestDiagnosticState(c).AccountFailover
					require.Equal(t, "window_legacy", d.Continuity.WindowState)
					require.True(t, d.Continuity.DeferredWindow)
					selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
					require.True(t, handled)
					encoded, _ := json.Marshal(d)
					require.NotNil(t, selected, "%s", encoded)
					require.Equal(t, target.ID(), selected.ID())
					h.store.Release(selected)
					require.Nil(t, h.commitSessionContinuity(c, selected))
					cleaned, headers, err := PrepareSessionRestartOutbound(c.Request.Context(), target, body, sessionFailoverRequestHeaders(c))
					require.NoError(t, err)
					assertSessionTools(t, cleaned)
					fingerprint := NewCodexTransportFingerprint(target, headers, cleaned, "", c.Request.Context())
					require.NoError(t, fingerprint.ClaimSessionIdentity(c.Request.Context(), target, "test-user-key"))
					require.Empty(t, fingerprint.accountWindowInputs, "legacy UUID must not invent a numbered window")
					record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, target.ID(), record.AccountID)
					require.Equal(t, owner.ID(), record.PreviousAccountID)
					require.EqualValues(t, 1, record.FailoverCount)
					require.Empty(t, record.OutboundWindowBases)
					h.continuityRecords = nil
					h.store.UnbindSessionAffinity(key, target.ID())
					resumed, _ := failoverTestRequest(t, h)
					resumed.Request.Header.Set(codexWindowIDHeader, continuityTestThread)
					require.Nil(t, h.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
					require.Equal(t, target.ID(), selectionTraceForRequest(resumed).PinnedAccount())
				})
			}
		}
	}
}

func TestRelaxedFailoverBlockedContinuityIsAudited(t *testing.T) {
	h, owner, _, key, c, body := relaxedOwnerRecoverySetup(t, true, true, "observe")
	c.Request.Header.Set(codexWindowIDHeader, "invalid-private-window")
	finish := h.beginServiceErrorAudit(c)
	failure := h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
	require.NotNil(t, failure)
	api.SendError(c, failure)
	finish()
	page := serviceErrorTestPage(t, h)
	require.Len(t, page.Items, 1)
	diagnostic := page.Items[0].AccountFailover
	require.Equal(t, "account_payment_required", diagnostic.TriggerReason)
	require.Equal(t, "invalid_session_continuity", diagnostic.BlockReason)
	require.Equal(t, "observe", diagnostic.Continuity.Mode)
	require.Equal(t, "window_invalid", diagnostic.Continuity.WindowState)
	require.Equal(t, owner.ID(), diagnostic.Continuity.PersistentAccountID)
	require.Equal(t, "persistent_binding", diagnostic.Continuity.OwnerSource)
}

func relaxedOwnerRecoverySetup(t *testing.T, persisted, missing bool, mode string) (*Handler, *auth.Account, *auth.Account, string, *gin.Context, []byte) {
	t.Helper()
	h, owner, target, key := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	config := h.store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = mode
	h.store.SetPromptFilterConfig(config)
	owner.Status, owner.CooldownReason, owner.CooldownUtil = auth.StatusCooldown, "payment_required", time.Now().Add(time.Hour)
	owner.CodexBPS, target.CodexBPS = true, true
	if !persisted {
		key += "-legacy"
		h.store.BindSessionAffinity(key, owner, "")
	}
	c, body := failoverTestRequest(t, h)
	if missing {
		body, _ = sjson.DeleteBytes(body, "client_metadata")
		c.Request.Header.Set("Session-Id", continuityTestThread)
		usageRequestDiagnosticState(c).Resolved = &usageRequestResolution{RootState: "resolved", Stable: true}
	} else {
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.window_id", continuityTestThread+":1")
	}
	body = addSessionTools(t, body)
	c.Set(ingressRequestBodyContextKey, body)
	return h, owner, target, key, c, body
}

func TestRelaxedFailoverRecoversMissingWindowAndPersistence(t *testing.T) {
	for _, persisted := range []bool{true, false} {
		for _, missing := range []bool{true, false} {
			for _, mode := range []string{"off", "observe"} {
				t.Run(fmt.Sprintf("persisted_%v/missing_window_%v/%s", persisted, missing, mode), func(t *testing.T) {
					h, owner, target, key, c, body := relaxedOwnerRecoverySetup(t, persisted, missing, mode)
					require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
					diagnostic := usageRequestDiagnosticState(c).AccountFailover
					require.Equal(t, "pending", diagnostic.Result)
					require.Equal(t, missing, diagnostic.Continuity.DeferredWindow)
					if !persisted {
						require.Equal(t, "persisted_existing_binding", diagnostic.Continuity.OwnerRecovery)
						require.Zero(t, diagnostic.Continuity.PersistentAccountID)
						require.Equal(t, "bps", diagnostic.PreviousUpstreamMode)
					}
					selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
					require.True(t, handled)
					require.Same(t, target, selected, "%+v", diagnostic)
					h.store.Release(selected)
					require.Nil(t, h.commitSessionContinuity(c, selected))
					cleaned, headers, err := PrepareSessionRestartOutbound(c.Request.Context(), target, body, c.Request.Header)
					require.NoError(t, err)
					assertSessionTools(t, cleaned)
					fingerprint := NewCodexTransportFingerprint(target, headers, cleaned, "", c.Request.Context())
					require.NoError(t, fingerprint.ClaimSessionIdentity(c.Request.Context(), target, "test-user-key"))
					record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, target.ID(), record.AccountID)
					require.Equal(t, owner.ID(), record.PreviousAccountID)
					require.EqualValues(t, 1, record.FailoverCount)
					require.True(t, record.OutboundWindowReset)
					if missing {
						require.Empty(t, record.OutboundWindowBases)
						require.Empty(t, record.OutboundWindows)
						require.False(t, gjson.GetBytes(fingerprint.ApplyBody(cleaned), "client_metadata.x-codex-window-id").Exists())
						if !persisted {
							require.False(t, record.NumberKnown)
						}
					}
					// A subsequent real window starts its own stable outbound numbering;
					// no blank-thread :0 baseline was created by the header-only request.
					numbers, err := h.db.ResolveSessionOutboundWindowNumbers(t.Context(), hashRiskIdentity(key), target.ID(), 1,
						map[string]database.SessionOutboundWindowInput{continuityTestThread: {Number: 1}})
					require.NoError(t, err)
					require.EqualValues(t, 0, numbers[continuityTestThread])
					h.continuityRecords = nil
					h.store.UnbindSessionAffinity(key, target.ID())
					resumed, raw := failoverTestRequest(t, h)
					if missing {
						raw, _ = sjson.DeleteBytes(raw, "client_metadata")
						resumed.Request.Header.Set("Session-Id", continuityTestThread)
					}
					require.Nil(t, h.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, raw))
					require.Equal(t, target.ID(), selectionTraceForRequest(resumed).PinnedAccount())
				})
			}
		}
	}
}

func TestRelaxedFailoverMissingMetadataKeepsGuards(t *testing.T) {
	for _, scenario := range []string{"enforce", "off_switch", "invalid_window", "thread_conflict", "empty_input", "owner_conflict", "storage_error"} {
		t.Run(scenario, func(t *testing.T) {
			mode := "observe"
			if scenario == "enforce" {
				mode = "enforce"
			}
			h, owner, target, key, c, body := relaxedOwnerRecoverySetup(t, true, true, mode)
			switch scenario {
			case "off_switch":
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
					s.CodexForkAccountFallbackEnabled = false
					s.CodexSessionFailoverEnabled = true
					return s
				})
			case "invalid_window":
				c.Request.Header.Set(codexWindowIDHeader, "malformed")
			case "thread_conflict":
				c.Request.Header.Set(codexThreadIDHeader, "01a03bb0-9da5-7772-a16a-f38258dd30c5")
			case "empty_input":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[]`))
			case "owner_conflict":
				h.store.UnbindSessionAffinity(key, owner.ID())
				h.store.BindSessionAffinity(key, target, "")
			case "storage_error":
				failedDB, err := database.New("sqlite", ":memory:")
				require.NoError(t, err)
				require.NoError(t, failedDB.Close())
				h.db = failedDB
			}
			c.Set(ingressRequestBodyContextKey, body)
			require.NotNil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			if scenario != "storage_error" {
				record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, owner.ID(), record.AccountID)
				require.Zero(t, record.FailoverCount)
			}
		})
	}
}

func TestRelaxedFailoverLegacyBindingRaces(t *testing.T) {
	for _, scenario := range []string{"changed_live_owner", "removed_binding", "persisted_other_owner"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, target, key, c, body := relaxedOwnerRecoverySetup(t, false, false, "observe")
			require.Nil(t, h.prepareSessionContinuity(c, requestSessionIdentity{stableIdentity: true}, key, body))
			switch scenario {
			case "changed_live_owner":
				h.store.UnbindSessionAffinity(key, owner.ID())
				h.store.BindSessionAffinity(key, target, "")
			case "removed_binding":
				h.store.UnbindSessionAffinity(key, owner.ID())
			case "persisted_other_owner":
				_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: target.ID(), LastSeen: time.Now()})
				require.NoError(t, err)
			}
			pending, failure := h.prepareSessionAccountFailover(c, key, body, auth.DispatchPolicyStandard)
			require.False(t, pending)
			require.NotNil(t, failure)
			record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.Equal(t, scenario == "persisted_other_owner", found)
			if found {
				require.Equal(t, target.ID(), record.AccountID)
				require.Zero(t, record.FailoverCount)
			}
		})
	}
}

func TestRelaxedFailoverMissingWindowQueuedSwitchHasSingleWinner(t *testing.T) {
	h, owner, target, key, first, body := relaxedOwnerRecoverySetup(t, true, true, "observe")
	second, _ := failoverTestRequest(t, h)
	second.Request.Header.Set("Session-Id", continuityTestThread)
	for _, c := range []*gin.Context{first, second} {
		require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	}
	var winners atomic.Int32
	done := make(chan struct{}, 2)
	for _, c := range []*gin.Context{first, second} {
		go func() {
			selected, _, _ := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			if selected != nil {
				winners.Add(1)
				h.store.Release(selected)
			}
			done <- struct{}{}
		}()
	}
	<-done
	<-done
	require.EqualValues(t, 1, winners.Load())
	record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, target.ID(), record.AccountID)
	require.Equal(t, owner.ID(), record.PreviousAccountID)
	require.EqualValues(t, 1, record.FailoverCount)
}
