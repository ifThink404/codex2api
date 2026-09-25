package proxy

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestBackgroundWaitRelaxedFallbackAfterOwnerChange(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, scenario := range []string{"owner_changed", "timeout", "canceled", "ready", "ticket"} {
			t.Run(fmt.Sprint(enabled)+"/"+scenario, func(t *testing.T) {
				h := newRootlessPassiveModelTestHandler(t)
				previous := CurrentRuntimeSettings()
				t.Cleanup(func() { ApplyRuntimeSettings(previous) })
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = enabled; return s })
				synctest.Test(t, func(t *testing.T) {
					parent := &auth.Account{DBID: 17, AccessToken: "root", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 2}
					target := &auth.Account{DBID: 18, AccessToken: "target", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 2}
					h.store.AddAccount(parent)
					h.store.AddAccount(target)
					remaining := int64(2000)
					fingerprint := promptSessionTestFingerprint(t.Name())
					meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
						RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: fingerprint,
						ThreadSource: "memory_consolidation", RequestKind: "memory", PassiveFeature: newAPIPassiveFeatureRelatedInternal, RootAccountWaitMillis: &remaining}
					if scenario == "ticket" {
						meta.WindowGrant = "explicit-ticket"
					}
					body := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"background request"}]}`)
					c, _ := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, meta)
					c.Set(ingressRequestBodyContextKey, body)
					h.primeNewAPIPolicyContext(c, body)
					rootKey := sessionAffinityKey("newapi-root-session:"+fingerprint, 101)
					entry := sessionContinuityCacheEntry{Record: database.SessionContinuityRecord{AccountID: parent.ID(), UpstreamMode: "bps", LastSeen: time.Now()}}
					h.cacheSessionContinuity(hashRiskIdentity(rootKey), entry)
					h.store.BindSessionAffinity(rootKey, parent, "")
					require.True(t, h.store.RemoveAccountSession(parent.ID(), rootKey))
					identity := h.resolveRequestSessionIdentityForContext(c, body)
					require.True(t, identity.requiresRootAccount)
					require.Nil(t, relaxedAccountFallbackFromContext(c.Request.Context()))
					ctx, cancel := context.WithCancel(c.Request.Context())
					defer cancel()
					c.Request = c.Request.WithContext(ctx)
					result := make(chan *api.APIError, 1)
					go func() { result <- h.waitForBackgroundRootWithFallback(c, &identity, body) }()
					synctest.Wait()
					if scenario != "ticket" {
						require.Empty(t, result)
					}
					switch scenario {
					case "owner_changed":
						h.store.UnbindSessionAffinity(rootKey, parent.ID())
						entry.Record.AccountID = target.ID()
						h.cacheSessionContinuity(hashRiskIdentity(rootKey), entry)
						h.store.BindSessionAffinity(rootKey, target, "")
					case "canceled":
						cancel()
					case "ready":
						require.True(t, h.store.AdmitAccountSession(parent, rootKey, time.Now()))
					}
					failure := <-result
					wantFallback := enabled && (scenario == "owner_changed" || scenario == "timeout")
					fallback := relaxedAccountFallbackFromContext(c.Request.Context())
					require.Equal(t, wantFallback, fallback != nil)
					if wantFallback {
						require.Nil(t, failure)
						require.False(t, identity.requiresRootAccount)
						require.False(t, identity.ownsRootBinding)
						require.NotEqual(t, rootKey, capacityAwareSessionAffinityKey(identity, 101))
						require.Nil(t, backgroundAccountMatchFromContext(c.Request.Context()))
						require.Nil(t, outboundEpochFromContext(c.Request.Context()))
						require.False(t, usageRequestDiagnosticState(c).Resolved.Related)
						require.False(t, usageRequestDiagnosticState(c).Resolved.RequiresRootAccount)
						require.Equal(t, "relaxed_fallback", usageRequestDiagnosticState(c).BackgroundWindowWait.Result)
						// Detaching never changes or releases the parent's current owner.
						stored, _, err := h.readSessionContinuity(t.Context(), hashRiskIdentity(rootKey))
						require.NoError(t, err)
						require.Equal(t, entry.Record.AccountID, stored.Record.AccountID)
						h.cleanupRelaxedAccountFallback(c)
					} else if scenario == "ready" {
						require.Nil(t, failure)
					} else {
						require.NotNil(t, failure)
					}
				})
			})
		}
	}
}

func TestBackgroundWaitRelaxedHTTPDispatch(t *testing.T) {
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			h, parent, target, _ := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			rootKey := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint("relaxed-parent"), 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(rootKey), database.SessionContinuityRecord{AccountID: parent.ID(), LastSeen: time.Now()})
			require.NoError(t, err)
			parent.SessionCapacityEnabled, parent.SessionCapacityMax = true, 2
			target.SessionCapacityEnabled, target.SessionCapacityMax = true, 2
			h.store.BindSessionAffinity(rootKey, parent, "")
			require.True(t, h.store.RemoveAccountSession(parent.ID(), rootKey))
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wire, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Contains(t, string(wire), "background task")
				require.Contains(t, r.Header.Get("Authorization"), "target-token")
				response := `{"id":"resp_test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
				if strings.HasSuffix(r.URL.Path, "/compact") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, response)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+response+"}\n\n")
				}
			}))
			t.Cleanup(server.Close)
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "wait-fallback-test"})
			body := []byte(`{"model":"gpt-5.6-sol","input":"background task","stream":true}`)
			if path == "/v1/messages" || path == "/v1/chat/completions" {
				body = []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"background task"}],"max_tokens":16,"stream":true}`)
			}
			c, recorder := relaxedTestRequest(t, h, "thread_title", "resolved", body, path)
			map[string]func(*gin.Context){"/v1/responses": h.Responses, "/v1/responses/compact": h.ResponsesCompact, "/v1/chat/completions": h.ChatCompletions, "/v1/messages": h.Messages}[path](c)
			require.Equal(t, 200, recorder.Code, recorder.Body.String())
			require.Equal(t, 1, calls, recorder.Body.String())
			state := usageRequestDiagnosticState(c).RelaxedFallback
			require.NotNil(t, state)
			require.Equal(t, "passive_wait_timeout", state.Reason)
			require.Equal(t, target.ID(), state.AccountID)
			_, bound := h.store.SessionAffinityAccountID(state.key)
			require.False(t, bound)
			original, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(rootKey))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, parent.ID(), original.AccountID)
		})
	}
}
