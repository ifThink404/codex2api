package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func relaxedTestRequest(t *testing.T, h *Handler, source, state string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	remaining := int64(0)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: state, RootSessionRelation: newAPIPolicyRootSessionRelationRelated,
		RootSessionFingerprint: promptSessionTestFingerprint("relaxed-parent"), ThreadSource: source, RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal, RootAccountWaitMillis: &remaining}
	if state != "resolved" {
		meta.RootSessionFingerprint = ""
		meta.PassiveFeature = ""
		meta.RootSessionRelation = ""
	}
	c, recorder := signedRootlessPassiveModelContext(t, "POST", "/v1/responses", body, meta)
	setSignedNewAPIRequestHeaders(t, c.Request, body, NewUpstreamSessionUUID(), newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "test-platform", "integration-secret", promptSessionTestFingerprint(t.Name()))
	meta.PlatformID, meta.Profile, meta.Mode = "test-platform", "balanced", "enforce"
	meta.Provider, meta.Protocol = "openai", "responses"
	meta.TokenID, meta.InstallationID = 7, "rootless-device"
	meta.SessionFingerprint = promptSessionTestFingerprint(t.Name())
	addSignedNewAPIPolicyMeta(t, c, meta, true)
	h.primeNewAPIPolicyContext(c, body)
	return c, recorder
}

func TestRelaxedFallbackPassiveParentAvailability(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, scenario := range []string{"unresolved", "missing", "removed", "healthy", "full", "active_full", "disabled", "paused", "quota_5h", "quota_7d", "quota_exempt", "auto_paused", "transient", "storage_error", "conflict", "user"} {
			t.Run(fmt.Sprint(enabled)+"/"+scenario, func(t *testing.T) {
				h, parent, _, _ := failoverTestSetup(t, false)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = enabled; return s })
				rootKey := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint("relaxed-parent"), 101)
				state, source := "resolved", "thread_title"
				switch scenario {
				case "unresolved":
					state = "unavailable"
				case "conflict":
					state = "conflict"
				case "user":
					source = "user"
				case "healthy", "full", "active_full", "disabled", "paused", "quota_5h", "quota_7d", "quota_exempt", "auto_paused", "transient", "removed":
					id := parent.ID()
					if scenario == "removed" {
						id = 99999
					}
					_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(rootKey), database.SessionContinuityRecord{AccountID: id, UpstreamMode: "bps", LastSeen: time.Now()})
					require.NoError(t, err)
					if scenario == "full" || scenario == "active_full" {
						parent.SessionCapacityEnabled, parent.SessionCapacityMax = true, 1
						occupied := "occupied"
						if scenario == "active_full" {
							occupied = rootKey
						}
						require.True(t, h.store.AdmitAccountSession(parent, occupied, time.Now()))
					}
					if scenario == "disabled" {
						atomic.StoreInt32(&parent.Disabled, 1)
					}
					if scenario == "paused" {
						atomic.StoreInt32(&parent.DispatchPaused, 1)
					}
					if scenario == "quota_5h" {
						parent.PlanType = "pro"
						parent.SetUsageSnapshot5h(100, time.Now().Add(time.Hour))
					}
					if scenario == "quota_7d" || scenario == "quota_exempt" {
						parent.UsagePercent7d, parent.UsagePercent7dValid, parent.Reset7dAt = 100, true, time.Now().Add(time.Hour)
					}
					if scenario == "quota_exempt" {
						parent.CodexUsageLimitBypassEnabled, parent.CodexUsageLimitBypassModels = true, []string{"gpt-5.6-sol"}
					}
					if scenario == "auto_paused" {
						h.store.SetGlobalAutoPauseThresholds(0, .8)
						parent.UsagePercent7d, parent.UsagePercent7dValid, parent.Reset7dAt = 85, true, time.Now().Add(time.Hour)
					}
					if scenario == "transient" {
						parent.Status, parent.CooldownReason, parent.CooldownUtil = auth.StatusCooldown, "server_error", time.Now().Add(time.Minute)
					}
				}
				body := []byte(`{"model":"gpt-5.6-sol","input":"background task"}`)
				c, _ := relaxedTestRequest(t, h, source, state, body)
				if scenario == "storage_error" {
					ctx, cancel := context.WithCancel(c.Request.Context())
					cancel()
					c.Request = c.Request.WithContext(ctx)
				}
				identity := h.resolveRequestSessionIdentityForContext(c, body)
				fallback := relaxedAccountFallbackFromContext(c.Request.Context())
				want := enabled && (scenario == "unresolved" || scenario == "missing" || scenario == "removed" || scenario == "full" || scenario == "disabled" || scenario == "paused" || scenario == "quota_5h" || scenario == "quota_7d" || scenario == "auto_paused")
				require.Equal(t, want, fallback != nil)
				if want {
					require.False(t, identity.requiresRootAccount)
					require.False(t, identity.stableIdentity)
					require.False(t, identity.protectedRelatedLease)
					require.NotEqual(t, rootKey, capacityAwareSessionAffinityKey(identity, 101))
					require.Nil(t, h.waitForBackgroundRootAccount(c, identity))
					if scenario == "removed" {
						floor, _ := c.Request.Context().Value(codexRouteFloorKey{}).(string)
						require.Equal(t, "bps", floor)
						filter := codexRouteAccountFilter(c, nil)
						require.False(t, filter(parent), "a deleted BPS parent must not send the child to Codex")
					}
				} else if source != "user" {
					require.True(t, identity.requiresRootAccount, "%+v", identity)
				}
			})
		}
	}
}

func TestRelaxedFallbackRequestLocalAdmissionAndRelease(t *testing.T) {
	h, parent, target, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	target.SessionCapacityEnabled, target.SessionCapacityMax = true, 1
	body := addSessionTools(t, []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"background task"}]}`))
	c, _ := relaxedTestRequest(t, h, "guardian_review", "resolved", body)
	c.Set(ingressRequestBodyContextKey, body)
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	key := capacityAwareSessionAffinityKey(identity, 101)
	require.Nil(t, h.configureSessionModelAffinity(c, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	filter := h.applyPassiveInternalModelRouting(c, "gpt-5.6-sol", identity, key, false, accountFilterForModel("gpt-5.6-sol"))
	selected, _, _ := h.nextAccountForSessionWithDispatchGuard(key, 101, map[int64]bool{parent.ID(): true}, filter, auth.DispatchPolicyStandard, selectionTraceForRequest(c))
	require.Same(t, target, selected)
	defer h.store.Release(selected)
	status, blocked := h.checkPromptSessionCreationLimitForSelectedAccountAdmission(c, body, selected, key, 0)
	require.False(t, blocked, "%+v", status)
	h.bindAccountSession(c, key, selected, "")
	_, bound := h.store.SessionAffinityAccountID(key)
	require.True(t, bound)
	_, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.False(t, found, "temporary requests must not create durable roots")
	rootKey := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint("relaxed-parent"), 101)
	// A real parent appearing during the temporary request must remain intact.
	h.store.BindSessionAffinity(rootKey, parent, "")
	clean, _, err := PrepareSessionRestartOutbound(c.Request.Context(), selected, body, c.Request.Header)
	require.NoError(t, err)
	assertSessionTools(t, clean)
	h.cleanupRelaxedAccountFallback(c)
	_, bound = h.store.SessionAffinityAccountID(key)
	require.False(t, bound)
	_, bound = h.store.AccountSessionAccountID(key, time.Now())
	require.False(t, bound)
	owner, bound := h.store.SessionAffinityAccountID(rootKey)
	require.True(t, bound)
	require.Equal(t, parent.ID(), owner)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = false; return s })
	c2, _ := relaxedTestRequest(t, h, "guardian_review", "resolved", body)
	identity2 := h.resolveRequestSessionIdentityForContext(c2, body)
	require.True(t, identity2.requiresRootAccount, "%+v", identity2)
	require.Nil(t, relaxedAccountFallbackFromContext(c2.Request.Context()))
}

func TestRelaxedFallbackPausedParentKeepsOriginalOwnership(t *testing.T) {
	h, parent, target, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	atomic.StoreInt32(&parent.DispatchPaused, 1)
	rootKey := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint("relaxed-parent"), 101)
	_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(rootKey), database.SessionContinuityRecord{AccountID: parent.ID(), Number: 27, NumberKnown: true, LastSeen: time.Now()})
	require.NoError(t, err)
	h.store.BindSessionAffinity(rootKey, parent, "")
	body := []byte(`{"model":"gpt-5.6-sol","input":"temporary background task"}`)
	c, _ := relaxedTestRequest(t, h, "thread_title", "resolved", body)
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	state := relaxedAccountFallbackFromContext(c.Request.Context())
	require.NotNil(t, state)
	require.Equal(t, "passive_parent_paused", state.Reason)
	key := capacityAwareSessionAffinityKey(identity, 101)
	require.Nil(t, h.configureSessionModelAffinity(c, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	filter := h.applyPassiveInternalModelRouting(c, "gpt-5.6-sol", identity, key, false, accountFilterForModel("gpt-5.6-sol"))
	// Even if the administrator resumes the parent during scheduling, the
	// already-detached request must not reclaim its old root or identity.
	atomic.StoreInt32(&parent.DispatchPaused, 0)
	require.False(t, filter(parent))
	selected, _, _ := h.nextAccountForSessionWithDispatchGuard(key, 101, nil, filter, auth.DispatchPolicyStandard, selectionTraceForRequest(c))
	require.Same(t, target, selected)
	defer h.store.Release(selected)
	status, blocked := h.checkPromptSessionCreationLimitForSelectedAccountAdmission(c, body, selected, key, 0)
	require.False(t, blocked, "%+v", status)
	h.bindAccountSession(c, key, selected, "")
	h.cleanupRelaxedAccountFallback(c)
	stored, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(rootKey))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, parent.ID(), stored.AccountID)
	require.EqualValues(t, 27, stored.Number)
	require.Zero(t, stored.FailoverCount)
	owner, found := h.store.SessionAffinityAccountID(rootKey)
	require.True(t, found)
	require.Equal(t, parent.ID(), owner)
}

func TestRelaxedFallbackRetainsWindowAuthorization(t *testing.T) {
	h, _, _, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	grant := quoteWindowAuthorization(t, h, "missing-parent-ticket", "reservation", "user")
	c := windowAuthorizationRequest(t, h, grant, "thread_title")
	body := []byte(`{"model":"gpt-5.6-sol","input":"test"}`)
	// The signed ticket exists before validateRequestWindowGrant installs the
	// decoded ticket on the context. Neither stage permits temporary selection.
	require.Nil(t, windowGrantForRequest(c))
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	require.True(t, identity.requiresRootAccount)
	require.Nil(t, relaxedAccountFallbackFromContext(c.Request.Context()))
}

func TestRelaxedFallbackRejectsUnverifiedSourceLabel(t *testing.T) {
	h, _, _, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	identity := requestSessionIdentity{requiresRootAccount: true}
	root := requestRootSessionIdentity{threadSource: "thread_title"}
	got := h.configureRelaxedAccountFallback(c, []byte(`{"input":"test"}`), identity, root)
	require.True(t, got.requiresRootAccount)
	require.Nil(t, relaxedAccountFallbackFromContext(c.Request.Context()))
}

func TestRelaxedFallbackHTTPDispatch(t *testing.T) {
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	for _, source := range []string{"thread_title", "ambient_suggestions", "agent_created_thread", "guardian_review", "memory_consolidation", "subagent"} {
		t.Run(source, func(t *testing.T) {
			h, parent, target, _ := failoverTestSetup(t, false)
			atomic.StoreInt32(&parent.Disabled, 1)
			target.SessionCapacityEnabled, target.SessionCapacityMax = true, 1
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wire, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Contains(t, string(wire), "background task")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			}))
			t.Cleanup(server.Close)
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "relaxed-test"})
			body := []byte(`{"model":"gpt-5.6-sol","input":"background task","stream":true}`)
			c, recorder := relaxedTestRequest(t, h, source, "resolved", body)
			h.Responses(c)
			require.Equal(t, 200, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "response.completed")
			require.Equal(t, 1, calls)
			state := usageRequestDiagnosticState(c).RelaxedFallback
			require.NotNil(t, state)
			require.Equal(t, target.ID(), state.AccountID)
			_, bound := h.store.SessionAffinityAccountID(state.key)
			require.False(t, bound)
		})
	}
}

func TestRelaxedFallbackContextAndModelRestrictions(t *testing.T) {
	h, _, target, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	body := addSessionTools(t, []byte(`{"model":"gpt-5.6-sol","input":[{"type":"reasoning","encrypted_content":"old-encrypted-state"},{"role":"user","content":"keep this"}],"client_metadata":{"parent_thread_id":"parent-value"}}`))
	c, _ := relaxedTestRequest(t, h, "thread_title", "unavailable", body)
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	key := capacityAwareSessionAffinityKey(identity, 101)
	require.Nil(t, h.configureSessionModelAffinity(c, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	copyBefore := bytes.Clone(body)
	clean, _, err := prepareRelaxedAccountOutbound(c.Request.Context(), body, c.Request.Header)
	require.NoError(t, err)
	require.NotContains(t, string(clean), "old-encrypted-state")
	require.Empty(t, gjson.GetBytes(clean, "client_metadata.parent_thread_id").String())
	require.Equal(t, copyBefore, body)
	assertSessionTools(t, clean)
	h.store.SetPassiveInternalModelsEnabled(false)
	filter := h.applyPassiveInternalModelRouting(c, "disallowed", identity, key, false, func(*auth.Account) bool { return false })
	require.False(t, filter(target), "relaxed scheduling must not enable a disabled model exemption")
	state := relaxedAccountFallbackFromContext(c.Request.Context())
	state.preserveInput = true
	_, _, err = prepareRelaxedAccountOutbound(c.Request.Context(), []byte(`{"input":"keep this","previous_response_id":"old-response"}`), nil)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "previous_response_id"))
}
