package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func forkFallbackSetup(t *testing.T, enabled bool, parentState, mode string) (*Handler, *auth.Account, *auth.Account, string, string) {
	t.Helper()
	h, parent, target, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = enabled; return s })
	cfg := h.store.GetPromptFilterConfig()
	cfg.Advanced.Risk.SessionContinuityMode = mode
	h.store.SetPromptFilterConfig(cfg)
	source, child := sessionAffinityKey(accountIdentitySampleRoot, 0), sessionAffinityKey(continuityTestThread, 0)
	if parentState != "missing" {
		id := parent.ID()
		if parentState == "removed" {
			id = 99999
		}
		_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(source), database.SessionContinuityRecord{AccountID: id, ThreadID: accountIdentitySampleRoot, Number: 55, NumberKnown: true, LastSeen: time.Now()})
		require.NoError(t, err)
	}
	if parentState == "full" {
		parent.SessionCapacityEnabled, parent.SessionCapacityMax, parent.SessionCapacityIdleTTLSeconds = true, 1, 60
		require.True(t, h.store.AdmitAccountSession(parent, "occupied-parent", time.Now()))
	}
	return h, parent, target, source, child
}

func TestForkAccountFallbackExecutorDetachesOnlyOutboundParent(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprint(preserve), func(t *testing.T) {
			h, _, target, _, child := forkFallbackSetup(t, true, "full", "enforce")
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionFailoverPreserveInput = preserve; return s })
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			type capture struct {
				body    []byte
				headers http.Header
			}
			seen := make(chan capture, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- capture{readUpstreamRequestBody(r), r.Header.Clone()}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"response-fork-fixture","output":[]}`))
			}))
			t.Cleanup(server.Close)
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "fork-fallback"})
			var session string
			for i := 0; i < 3; i++ {
				c, body := newForkFallbackRequest(t, h, false)
				c.Request.Header.Set(codexParentThreadIDHeader, accountIdentitySampleRoot)
				c.Request.Header.Set("X-Codex-Forked-From-Thread-Id", accountIdentitySampleRoot)
				for _, field := range []string{"parent_thread_id", "forked_from_thread_id", "guardian_classifier_source_thread_id"} {
					body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata."+field, accountIdentitySampleRoot)
				}
				body, _ = sjson.SetBytes(body, "client_metadata.x-codex-forked-from-thread-id", accountIdentitySampleRoot)
				c.Request.Header.Set(codexTurnMetadataHeader, gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Raw)
				c.Set(ingressRequestBodyContextKey, body)
				original, originalHeaders := bytes.Clone(body), c.Request.Header.Clone()
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: accountIdentitySampleRoot}, child, "gpt-5.6-sol", "gpt-5.6-sol", i == 2, body))
				require.Nil(t, h.commitSessionContinuity(c, target))
				var response *http.Response
				var err error
				if i == 2 {
					response, err = ExecuteCompactRequest(c.Request.Context(), target, body, "cache", "", "test-user-key", nil, c.Request.Header)
				} else {
					response, err = ExecuteRequest(c.Request.Context(), target, body, "cache", "", "test-user-key", nil, c.Request.Header, false)
				}
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.NoError(t, response.Body.Close())
				sent := <-seen
				require.Empty(t, codexAccountIdentityReferences(sent.headers, sent.body))
				require.NotEmpty(t, sent.headers.Get(codexSessionIDHeader))
				if i == 0 {
					session = sent.headers.Get(codexSessionIDHeader)
				} else {
					require.Equal(t, session, sent.headers.Get(codexSessionIDHeader))
				}
				require.Equal(t, session+":0", sent.headers.Get(codexWindowIDHeader))
				assertSessionTools(t, sent.body)
				require.JSONEq(t, gjson.GetBytes(original, "input").Raw, gjson.GetBytes(sent.body, "input").Raw)
				require.Equal(t, original, body)
				require.Equal(t, originalHeaders, c.Request.Header)
				record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(child))
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, []string{accountIdentitySampleRoot}, record.DetachedForkReferences)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
					s.CodexForkAccountFallbackEnabled = false
					s.CodexSessionFailoverPreserveInput = !preserve
					return s
				})
				h.continuityRecords = nil
			}
		})
	}
}

func TestForkAccountFallbackConcurrentBindingSingleWinner(t *testing.T) {
	h, parent, target, _, child := forkFallbackSetup(t, true, "missing", "enforce")
	root := hashRiskIdentity(child)
	var records [2]database.SessionContinuityRecord
	var failures [2]error
	var workers sync.WaitGroup
	for i, account := range []*auth.Account{parent, target} {
		workers.Go(func() {
			next := database.SessionContinuityRecord{AccountID: account.ID(), ThreadID: continuityTestThread, Number: 27, NumberKnown: true, LastSeen: time.Now(), LastFailoverReason: "continuity_fork_parent_missing", DetachedForkReferences: []string{accountIdentitySampleRoot}, PreserveRestartInput: true}
			records[i], failures[i] = h.db.RestartSessionContinuity(t.Context(), root, database.SessionContinuityRecord{}, next)
		})
	}
	workers.Wait()
	successes := 0
	for i, err := range failures {
		if err == nil {
			successes++
			require.EqualValues(t, 1, records[i].FailoverCount)
			require.True(t, records[i].PreserveRestartInput)
			require.Equal(t, []string{accountIdentitySampleRoot}, records[i].DetachedForkReferences)
		} else {
			require.ErrorIs(t, err, database.ErrSessionOwnerConflict)
		}
	}
	require.Equal(t, 1, successes)
	stored, found, err := h.db.ReadSessionContinuity(t.Context(), root)
	require.NoError(t, err)
	require.True(t, found)
	_, err = h.db.RestartSessionContinuity(t.Context(), root, stored, stored)
	require.Error(t, err, "fork fallback must not restart an already-bound child")
}

func newForkFallbackRequest(t *testing.T, h *Handler, websocket bool) (*gin.Context, []byte) {
	t.Helper()
	c, body := outboundEpochTestRequest(t, h, 27)
	if websocket {
		c.Request.Method = "GET"
		c.Request.Header.Set("Connection", "Upgrade")
		c.Request.Header.Set("Upgrade", "websocket")
	}
	body = addSessionTools(t, body)
	return c, body
}

func TestForkAccountFallbackMissingFullAndDefaults(t *testing.T) {
	for _, mode := range []string{"off", "observe", "enforce"} {
		for _, enabled := range []bool{false, true} {
			for _, parentState := range []string{"missing", "removed", "full", "healthy"} {
				for _, ws := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/on=%t/%s/ws=%t", mode, enabled, parentState, ws), func(t *testing.T) {
						h, parent, target, source, child := forkFallbackSetup(t, enabled, parentState, mode)
						c, body := newForkFallbackRequest(t, h, ws)
						original := bytes.Clone(body)
						identity := requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: accountIdentitySampleRoot}
						failure := h.configureSessionModelAffinity(c, identity, child, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
						if !enabled && parentState != "healthy" {
							require.NotNil(t, failure)
							if parentState == "full" {
								require.Equal(t, "codex_sticky_account_capacity_full", string(failure.Code))
							} else {
								require.Equal(t, "codex_session_continuity_fork_owner_unavailable", string(failure.Code))
							}
							return
						}
						require.Nil(t, failure)
						trace := selectionTraceForRequest(c)
						filter := codexRouteAccountFilter(c, accountFilterForModel("gpt-5.6-sol"))
						selected, _ := h.takeForkSourceAccount(c.Request.Context(), identity, child, 0, nil, filter, auth.DispatchPolicyStandard, trace)
						if parentState == "healthy" {
							require.Equal(t, parent.ID(), trace.PinnedAccount())
							require.Same(t, parent, selected)
							require.Nil(t, forkAccountFallbackFromContext(c.Request.Context()))
							h.store.Release(selected)
							return
						}
						require.Nil(t, selected)
						require.Zero(t, trace.PinnedAccount())
						require.NotNil(t, forkAccountFallbackFromContext(c.Request.Context()))
						// The ordinary selector still enforces its request filter; make the
						// target the only authorized candidate in this fixture.
						filter = codexRouteAccountFilter(c, func(a *auth.Account) bool { return a.ID() == target.ID() })
						selected, _, _ = h.nextAccountForSessionWithDispatchGuard(child, 0, nil, filter, auth.DispatchPolicyStandard, trace)
						require.Same(t, target, selected)
						defer h.store.Release(selected)
						require.Nil(t, h.commitSessionContinuity(c, selected))
						record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(child))
						require.NoError(t, err)
						require.True(t, found)
						require.Equal(t, target.ID(), record.AccountID)
						require.True(t, record.LossyContextRestart)
						require.True(t, record.OutboundWindowReset)
						require.Equal(t, uint64(27), record.Number)
						cleaned, _, err := PrepareSessionRestartOutbound(c.Request.Context(), selected, body, c.Request.Header)
						require.NoError(t, err)
						assertSessionTools(t, cleaned)
						require.Equal(t, original, body)
						parentRecord, parentFound, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(source))
						require.NoError(t, err)
						if parentState == "missing" {
							require.False(t, parentFound)
						} else {
							require.True(t, parentFound)
							require.Equal(t, uint64(55), parentRecord.Number)
							require.Zero(t, parentRecord.FailoverCount)
						}
						// Turning the switch off cannot restore the parent or rotate this child.
						UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = false; return s })
						h.continuityRecords = nil
						c2, b2 := newForkFallbackRequest(t, h, ws)
						require.Nil(t, h.configureSessionModelAffinity(c2, identity, child, "gpt-5.6-sol", "gpt-5.6-sol", false, b2))
						require.Equal(t, target.ID(), selectionTraceForRequest(c2).PinnedAccount())
					})
				}
			}
		}
	}
}

func TestForkAccountFallbackContextAndBPSBoundary(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprint(preserve), func(t *testing.T) {
			h, parent, target, source, child := forkFallbackSetup(t, true, "missing", "enforce")
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionFailoverPreserveInput = preserve; return s })
			parent.SessionCapacityEnabled, parent.SessionCapacityMax, parent.SessionCapacityIdleTTLSeconds = true, 1, 60
			require.True(t, h.store.AdmitAccountSession(parent, "parent-occupied", time.Now()))
			native := true
			parent.CodexBPS, target.CodexBPS, target.CodexNative = true, true, &native
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(source), database.SessionContinuityRecord{AccountID: parent.ID(), ThreadID: accountIdentitySampleRoot, NumberKnown: true, UpstreamMode: "bps"})
			require.NoError(t, err)
			c, body := newForkFallbackRequest(t, h, false)
			body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"reasoning","encrypted_content":"parent-cipher"}`))
			c.Request.Header.Set(codexTurnStateHeader, "parent-turn-state")
			identity := requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: accountIdentitySampleRoot}
			require.Nil(t, h.configureSessionModelAffinity(c, identity, child, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			filter := codexRouteAccountFilter(c, nil)
			require.False(t, filter(&auth.Account{DBID: 99, AccessToken: "native-only"}))
			require.False(t, filter(&auth.Account{DBID: 100, UpstreamType: auth.UpstreamOpenAIResponses}))
			require.True(t, filter(target))
			routing, headers := sessionRestartRoutingContext(c, body)
			require.Empty(t, headers.Get(codexTurnStateHeader))
			require.Equal(t, preserve, bytes.Contains(routing, []byte("parent-cipher")))
			require.Nil(t, h.commitSessionContinuity(c, target))
			stored, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(child))
			require.NoError(t, err)
			require.Equal(t, preserve, stored.PreserveRestartInput)
			require.Equal(t, "bps", stored.UpstreamMode)
			cleaned, out, err := PrepareSessionRestartOutbound(c.Request.Context(), target, body, c.Request.Header)
			require.NoError(t, err)
			require.Empty(t, out.Get(codexTurnStateHeader))
			assertSessionTools(t, cleaned)
			require.Equal(t, preserve, bytes.Contains(cleaned, []byte("parent-cipher")))
		})
	}
}

func TestForkAccountFallbackAllowsUnboundCompactionAndPreservesOtherGuards(t *testing.T) {
	for _, scenario := range []string{"compaction", "existing", "background", "bad_window", "storage_error", "empty_context"} {
		t.Run(scenario, func(t *testing.T) {
			h, parent, _, _, child := forkFallbackSetup(t, true, "missing", "enforce")
			c, body := newForkFallbackRequest(t, h, false)
			identity := requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: accountIdentitySampleRoot}
			switch scenario {
			case "compaction":
				usageRequestDiagnosticState(c).Resolved.RequestKind = "compaction"
			case "existing":
				_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(child), database.SessionContinuityRecord{AccountID: parent.ID(), ThreadID: continuityTestThread, Number: 27, NumberKnown: true})
				require.NoError(t, err)
			case "background":
				identity.requiresRootAccount = true
				usageRequestDiagnosticState(c).Resolved.ThreadSource = "thread_title"
			case "bad_window":
				c.Request.Header.Set(codexWindowIDHeader, "invalid")
				body, _ = sjson.DeleteBytes(body, "client_metadata")
			case "storage_error":
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			case "empty_context":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","encrypted_content":"old"}]`))
			}
			failure := h.configureSessionModelAffinity(c, identity, child, "gpt-5.6-sol", "gpt-5.6-sol", scenario == "compaction", body)
			if scenario == "compaction" {
				// sever admits first-seen compaction without inventing a parent
				// binding or activating the lossy fork fallback.
				require.Nil(t, failure)
				require.Zero(t, selectionTraceForRequest(c).PinnedAccount())
			} else if scenario == "existing" {
				require.Nil(t, failure)
				require.Equal(t, parent.ID(), selectionTraceForRequest(c).PinnedAccount())
			} else if scenario == "background" {
				require.NotNil(t, h.waitForBackgroundRootAccount(c, identity))
			} else {
				require.NotNil(t, failure)
			}
			if scenario != "empty_context" {
				require.Nil(t, forkAccountFallbackFromContext(c.Request.Context()))
			}
		})
	}
}

func TestForkAccountFallbackWindowQuoteAndDispatch(t *testing.T) {
	for _, operation := range []string{"quote", "quote_tiered"} {
		for _, parentState := range []string{"missing", "full", "healthy_then_full"} {
			t.Run(operation+"/"+parentState, func(t *testing.T) {
				h, parent, target, _, _ := forkFallbackSetup(t, true, "missing", "enforce")
				parent.SessionCapacityEnabled, parent.SessionCapacityMax, parent.SessionCapacityIdleTTLSeconds = true, 1, 60
				target.SessionCapacityEnabled, target.SessionCapacityMax, target.SessionCapacityIdleTTLSeconds = true, 2, 60
				parentFingerprint, childFingerprint := promptSessionTestFingerprint("relaxed-parent"), promptSessionTestFingerprint("relaxed-child")
				if parentState != "missing" {
					source := sessionAffinityKey("newapi-root-session:"+parentFingerprint, 101)
					_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(source), database.SessionContinuityRecord{AccountID: parent.ID(), ThreadID: accountIdentitySampleRoot, NumberKnown: true})
					require.NoError(t, err)
				}
				if parentState == "full" {
					require.True(t, h.store.AdmitAccountSession(parent, "parent-occupied", time.Now()))
				}
				meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: childFingerprint, ForkedFromSessionFingerprint: parentFingerprint, ThreadSource: "user", RequestKind: "turn"}
				payload, err := json.Marshal(windowControlRequest{Operation: operation, Multiplier: 1, MultiplierStep: .5, ReservationID: "relaxed-quote"})
				require.NoError(t, err)
				control, w := windowExpansionTestContext(t, "/v1/session-windows", payload, meta)
				h.ControlNewAPIUserWindows(control)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				ticket := gjson.GetBytes(w.Body.Bytes(), "ticket").String()
				require.NotEmpty(t, ticket)
				grant, err := decodeWindowGrant("integration-secret", ticket)
				require.NoError(t, err)
				require.Zero(t, grant.Grant.OwnerAccountID)
				require.False(t, grant.Grant.Expanded)
				if parentState == "healthy_then_full" {
					require.True(t, h.store.AdmitAccountSession(parent, "filled-after-quote", time.Now()))
				}
				seed, body := newForkFallbackRequest(t, h, false)
				meta.WindowGrant = ticket
				c, _ := windowExpansionTestContext(t, "/v1/responses", body, meta)
				c.Set(ingressRequestBodyContextKey, body)
				h.primeNewAPIPolicyContext(c, body)
				usageRequestDiagnosticState(c).Resolved = usageRequestDiagnosticState(seed).Resolved
				beginDispatchSelection(c)
				require.Nil(t, h.requestWindowGrantError(c))
				child := sessionAffinityKey("newapi-root-session:"+childFingerprint, 101)
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: "newapi-root-session:" + parentFingerprint}, child, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				require.Zero(t, selectionTraceForRequest(c).PinnedAccount())
				require.NoError(t, h.bindWindowGrantOwner(c, target.ID(), child))
				require.Nil(t, h.commitSessionContinuity(c, target))
				require.Equal(t, target.ID(), windowGrantForRequest(c).Grant.OwnerAccountID)
				require.Equal(t, grant.Grant.ID, windowGrantForRequest(c).Grant.ID)
				require.Equal(t, grant.Grant.ExpiresAt, windowGrantForRequest(c).Grant.ExpiresAt)
			})
		}
	}
}
