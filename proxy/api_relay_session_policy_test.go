package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func apiRelayPolicyTestAccount() *auth.Account {
	return &auth.Account{DBID: 1258, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "relay-test-key", PlanType: "api", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, GroupIDs: []int64{2}}
}

func TestAPIRelayAccountCapacityKeepsRelatedAndNonAccountingScopes(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	relay := apiRelayPolicyTestAccount()
	relay.SessionCapacityEnabled, relay.SessionCapacityMax = true, 1
	handler.store.AddAccount(relay)
	now := time.Now()
	keyFor := func(identity requestSessionIdentity) string {
		return capacityAwareSessionAffinityKey(apiRelaySessionIdentity(identity), 7)
	}
	root := requestSessionIdentity{affinityID: "root-a", stableIdentity: true}
	require.True(test, handler.store.AdmitAccountSession(relay, keyFor(root), now))
	require.False(test, handler.store.AdmitAccountSession(relay, capacityAwareSessionAffinityKey(apiRelaySessionIdentity(root), 8), now), "another API key must not borrow the same root's slot")
	root.relatedToRoot = true
	root.protectedRelatedLease = true
	require.Equal(test, auth.RelatedSessionAffinityKey(sessionAffinityKey("root-a", 7)), keyFor(root), "API child requests must share the original root without native protected concurrency privileges")
	require.True(test, handler.store.AdmitAccountSession(relay, keyFor(root), now))
	require.False(test, handler.store.AdmitAccountSession(relay, keyFor(requestSessionIdentity{affinityID: "root-b", stableIdentity: true}), now))
	require.True(test, handler.store.AdmitAccountSession(relay, keyFor(requestSessionIdentity{affinityID: "background", stableIdentity: true, bypassWindowAccounting: true}), now))
	total, _ := handler.store.AccountSessionSlotCounts(relay.ID(), now)
	require.EqualValues(test, 1, total)
}

func TestAPIRelayHTTPEnforcesConfiguredAccountCapacity(test *testing.T) {
	var seenPath, seenAuth string
	var seenBody []byte
	upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer upstream.Close()
	handler := newWindowAuthorizationHandler(test)
	config := handler.store.GetPromptFilterConfig()
	config.Enabled, config.Advanced.NewAPI.Enabled = false, false
	handler.store.SetPromptFilterConfig(config)
	relay := apiRelayPolicyTestAccount()
	relay.BaseURL = upstream.URL
	relay.SessionCapacityEnabled, relay.SessionCapacityMax = true, 1
	handler.store.AddAccount(relay)
	for _, step := range []struct {
		root    string
		allowed bool
	}{{"api-window-a", true}, {"api-window-a", true}, {"api-window-b", false}} {
		body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Set(contextAPIKeyID, int64(7))
		request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		request.Request.Header.Set("Session-Id", step.root)
		seenBody = nil
		handler.Responses(request)
		if step.allowed {
			require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
			require.NotEmpty(test, seenBody)
		} else {
			require.NotEqual(test, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Empty(test, seenBody, "overflow request must not reach the upstream")
		}
		require.True(test, apiRelaySessionExempt(request), "account capacity must not remove the API user-window exemption")
	}
	total, _ := handler.store.AccountSessionSlotCounts(relay.ID(), time.Now())
	require.EqualValues(test, 1, total)
}

func TestAPIRelaySessionPolicyUsesAuthorizedRouteNotClientLabel(test *testing.T) {
	for _, scenario := range []string{"api_only", "mixed_unbound", "mixed_fresh", "mixed_api_owner", "mixed_codex_owner", "api_group", "api_not_allowed"} {
		test.Run(scenario, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Risk.SessionContinuityMode = "enforce"
			handler.store.SetPromptFilterConfig(config)
			relay := apiRelayPolicyTestAccount()
			codex := &auth.Account{DBID: 1695, AccessToken: "codex", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, GroupIDs: []int64{1}}
			handler.store.AddAccount(relay)
			if scenario != "api_only" {
				handler.store.AddAccount(codex)
			}
			request, body := continuityTestRequest(47, "turn")
			if scenario == "mixed_fresh" {
				request, body = continuityTestRequest(0, "turn")
			}
			request.Set(contextAPIKeyID, int64(7))
			request.Request.Header.Set("X-API-Session-Exempt", "true")
			identity := requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true, hasRequestFingerprint: true}
			key := sessionAffinityKey(identity.affinityID, 7)
			if strings.HasSuffix(scenario, "_owner") {
				owner := relay
				if scenario == "mixed_codex_owner" {
					owner = codex
				}
				_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, Number: 2, NumberKnown: true})
				require.NoError(test, err)
			}
			if scenario == "api_group" || scenario == "api_not_allowed" {
				groups := []int64{2}
				if scenario == "api_not_allowed" {
					groups = []int64{1}
				}
				request.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7, AllowedGroupIDs: groups})
				handler.store.SetAPIKeyAllowedGroups(7, groups)
			}
			resolved := handler.configureAPIRelaySessionPolicy(request, body, identity)
			exempt := scenario == "api_only" || scenario == "mixed_unbound" || scenario == "mixed_api_owner" || scenario == "api_group"
			require.Equal(test, exempt, apiRelaySessionExempt(request))
			if exempt {
				require.Equal(test, key, capacityAwareSessionAffinityKey(resolved, 7))
				require.False(test, resolved.requiresRootAccount)
				filter := applyAffinityGroupRouting(request, resolved, nil)
				require.True(test, filter(relay))
				require.False(test, filter(codex))
				require.Nil(test, handler.configureSessionModelAffinity(request, resolved, capacityAwareSessionAffinityKey(resolved, 7), "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				require.Nil(test, continuityRequest(request))
				require.Nil(test, outboundEpochFromContext(request.Request.Context()))
			} else {
				require.Equal(test, identity, resolved)
			}
		})
	}
}

func TestAPIRelaySessionKeepsRootAndExemptsExpiredUserWindowWithoutPassivePrivileges(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	relay := apiRelayPolicyTestAccount()
	handler.store.AddAccount(relay)
	handler.store.BindSessionAffinity(continuityTestThread, relay, "")
	request, body := continuityTestRequest(47, "turn")
	usageRequestDiagnosticState(request).StartedAt = time.Now()
	identity := requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true, relatedToRoot: true, requiresRootAccount: true, protectedRelatedLease: true}
	identity = handler.configureAPIRelaySessionPolicy(request, body, identity)
	require.True(test, apiRelaySessionExempt(request))
	require.False(test, passiveInternalRequestAuthorized(request))
	request.Set(windowGrantContextKey, &signedWindowGrant{Grant: database.UserWindowGrant{ID: "expired", Root: "old", Expanded: true, ExpiresAt: time.Now().Add(-time.Hour)}})
	require.Nil(test, handler.waitForBackgroundRootAccount(request, identity))
	require.True(test, identity.relatedToRoot)
	require.True(test, identity.requiresRootAccount)
	require.Equal(test, relay.ID(), usageRequestDiagnosticState(request).RootAccountID)
	require.Nil(test, handler.requestWindowGrantError(request))
	status, blocked := handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(request, body, relay, capacityAwareSessionAffinityKey(identity, 0), 0)
	require.False(test, blocked, "%+v", status)
	require.Nil(test, handler.confirmRequestWindowGrant(request))
	require.Nil(test, continuityRequest(request))
}

func TestAPIRelayHTTPEndpointsAllowNonzeroAndRootlessRequests(test *testing.T) {
	for _, transport := range []string{"http", "compact", "chat", "messages"} {
		for _, kind := range []string{"user", "thread_description"} {
			test.Run(transport+"/"+kind, func(test *testing.T) {
				var seenPath, seenAuth string
				var seenBody []byte
				upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
				if transport == "compact" {
					upstream.Close()
					upstream = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						seenBody = readUpstreamRequestBody(request)
						writer.Header().Set("Content-Type", "application/json")
						_, _ = writer.Write([]byte(`{"id":"resp_api_compact","object":"response","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
					}))
				}
				defer upstream.Close()
				handler := newWindowAuthorizationHandler(test)
				config := handler.store.GetPromptFilterConfig()
				config.Enabled = false
				config.Advanced.NewAPI.Enabled = false
				config.Advanced.Risk.SessionContinuityMode = "enforce"
				handler.store.SetPromptFilterConfig(config)
				relay := apiRelayPolicyTestAccount()
				relay.BaseURL = upstream.URL
				handler.store.AddAccount(relay)
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":true,"client_metadata":{"x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","window_id":"%s:47","window_number":47,"thread_source":"%s","request_kind":"turn"}}}`, continuityTestThread, continuityTestThread, continuityTestThread, kind))
				path := "/v1/responses"
				if transport == "compact" {
					path += "/compact"
				} else if transport == "chat" {
					path = "/v1/chat/completions"
				} else if transport == "messages" {
					path = "/v1/messages"
				}
				recorder := httptest.NewRecorder()
				request, _ := gin.CreateTestContext(recorder)
				request.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				if transport == "compact" {
					handler.ResponsesCompact(request)
				} else if transport == "chat" {
					handler.ChatCompletions(request)
				} else if transport == "messages" {
					handler.Messages(request)
				} else {
					handler.Responses(request)
				}
				require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
				require.True(test, apiRelaySessionExempt(request))
				require.NotEmpty(test, seenBody)
				_, found, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(continuityTestThread))
				require.NoError(test, err)
				require.False(test, found)
			})
		}
	}
}

func TestAPIRelaySessionPolicyUsesOriginalModelAlias(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	relay := apiRelayPolicyTestAccount()
	relay.Models = []string{"native-model"}
	relay.ModelMapping = `{"client-alias":"native-model"}`
	handler.store.AddAccount(relay)
	request, body := continuityTestRequest(47, "turn")
	cacheTrustedRequestedModel(request, "client-alias")
	identity := requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true}
	handler.configureAPIRelaySessionPolicy(request, body, identity)
	require.True(test, apiRelaySessionExempt(request))
}

func TestAPIRelayRetainsExistingSoftAffinityWithoutPersistentContinuity(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	relay := apiRelayPolicyTestAccount()
	handler.store.AddAccount(relay)
	request, body := continuityTestRequest(47, "turn")
	identity := requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true}
	handler.store.BindSessionAffinity(continuityTestThread, relay, "")
	identity = handler.configureAPIRelaySessionPolicy(request, body, identity)
	key := capacityAwareSessionAffinityKey(identity, 0)
	require.Nil(test, handler.configureSessionModelAffinity(request, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	accountID, found := handler.store.LiveSessionAccountID(key, time.Now())
	require.True(test, found)
	require.Equal(test, relay.ID(), accountID)
	_, found, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.False(test, found)
	// The original root remains the shared relay binding, even in a mixed pool.
	handler.store.AddAccount(&auth.Account{DBID: 1695, AccessToken: "codex", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady})
	request, body = continuityTestRequest(0, "turn")
	resolved := handler.configureAPIRelaySessionPolicy(request, body, requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true})
	require.True(test, apiRelaySessionExempt(request))
	require.Equal(test, key, capacityAwareSessionAffinityKey(resolved, 0))
}

func TestAPIRelayDoesNotRelaxCodexOnlyWebsocketEndpoint(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	handler.store.AddAccount(apiRelayPolicyTestAccount())
	request, body := continuityTestRequest(47, "turn")
	request.Request.Method = http.MethodGet
	request.Request.Header.Set("Connection", "Upgrade")
	request.Request.Header.Set("Upgrade", "websocket")
	identity := requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true}
	require.Equal(test, identity, handler.configureAPIRelaySessionPolicy(request, body, identity))
	require.False(test, apiRelaySessionExempt(request))
}
