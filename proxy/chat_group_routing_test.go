package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func chatGroupTestRequest(test *testing.T, handler *Handler, path string) (*gin.Context, []byte, requestSessionIdentity) {
	test.Helper()
	request, body := continuityTestRequest(0, "turn")
	request.Request.URL.Path = path
	request.Set(contextAPIKeyID, int64(7))
	request.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7, AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}}})
	handler.store.SetAPIKeyAllowedGroups(7, []int64{10})
	handler.store.SetAPIKeyNoAffinityGroups(7, []int64{20})
	return request, body, requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true, hasRequestFingerprint: true}
}

func TestChatGroupRoutingPathPriorityAndInnerFilter(test *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/responses/compact", "/v1/messages"} {
		test.Run(path, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			request, body, identity := chatGroupTestRequest(test, handler, path)
			primary := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
			split := &auth.Account{DBID: 2, GroupIDs: []int64{20}}
			handler.store.AddAccount(primary)
			handler.store.AddAccount(split)
			for _, fingerprint := range []bool{false, true} {
				identity.hasRequestFingerprint = fingerprint
				resolved := handler.configureAPIRelaySessionPolicy(request, body, identity)
				require.Equal(test, identity, resolved, "routing must not erase identity or fingerprint")
				filter := applyAffinityGroupRouting(request, resolved, nil)
				wantSplit := path == "/v1/chat/completions" || !fingerprint
				require.Equal(test, wantSplit, filter(split))
				require.Equal(test, !wantSplit, filter(primary))
				require.False(test, applyAffinityGroupRouting(request, resolved, func(*auth.Account) bool { return false })(split))
			}
			if path == "/v1/chat/completions" {
				require.Equal(test, "chat_completions_path", usageRequestDiagnosticState(request).GroupRouting.Reason)
			}
		})
	}
}

func TestChatGroupRoutingPreservesBoundCohortAndPermissions(test *testing.T) {
	for _, scenario := range []string{"persistent_primary", "persistent_split", "live_primary", "missing_account", "revoked_permission", "other_key", "different_root"} {
		test.Run(scenario, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			request, body, identity := chatGroupTestRequest(test, handler, "/v1/chat/completions")
			primary := &auth.Account{DBID: 1, GroupIDs: []int64{10}, AccessToken: "test", Models: []string{"gpt-5.6-sol"}}
			split := &auth.Account{DBID: 2, GroupIDs: []int64{20}, AccessToken: "test", Models: []string{"gpt-5.6-sol"}}
			handler.store.AddAccount(primary)
			handler.store.AddAccount(split)
			owner := primary
			if scenario == "persistent_split" {
				owner = split
			}
			keyID := int64(7)
			if scenario == "other_key" {
				keyID = 8
			}
			root := identity.affinityID
			if scenario == "different_root" {
				root = "another-root"
			}
			key := sessionAffinityKey(root, keyID)
			if scenario == "live_primary" {
				handler.store.BindSessionAffinity(key, owner, "")
			} else {
				ownerID := owner.ID()
				if scenario == "missing_account" {
					ownerID = 999
				}
				_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: ownerID, ThreadID: root, LastSeen: time.Now().Add(-24 * time.Hour)})
				require.NoError(test, err)
			}
			if scenario == "revoked_permission" {
				handler.store.SetAPIKeyAllowedGroups(7, []int64{30})
			}
			resolved := handler.configureAPIRelaySessionPolicy(request, body, identity)
			filter := applyAffinityGroupRouting(request, resolved, func(account *auth.Account) bool { return handler.store.APIKeyAllowsAccount(7, account) })
			switch scenario {
			case "missing_account", "revoked_permission":
				require.False(test, filter(primary))
				require.False(test, filter(split))
			case "other_key", "different_root", "persistent_split":
				require.True(test, filter(split))
				require.False(test, filter(primary))
			default:
				require.True(test, filter(primary))
				require.False(test, filter(split))
			}
			serialized, err := json.Marshal(usageRequestDiagnosticState(request))
			require.NoError(test, err)
			require.Contains(test, string(serialized), `"group_routing"`)
			require.NotContains(test, string(serialized), `"groups"`)
		})
	}
}

func TestChatGroupRoutingUsesSamePoolForAPIRelayExemption(test *testing.T) {
	for _, existing := range []bool{false, true} {
		test.Run(map[bool]string{false: "new", true: "bound_codex"}[existing], func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			request, body, identity := chatGroupTestRequest(test, handler, "/v1/chat/completions")
			codex := &auth.Account{DBID: 1, GroupIDs: []int64{10}, AccessToken: "test", Models: []string{"gpt-5.6-sol"}}
			relay := apiRelayPolicyTestAccount()
			relay.GroupIDs = []int64{20}
			handler.store.AddAccount(codex)
			handler.store.AddAccount(relay)
			if existing {
				_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(sessionAffinityKey(identity.affinityID, 7)), database.SessionContinuityRecord{AccountID: codex.ID()})
				require.NoError(test, err)
			}
			resolved := handler.configureAPIRelaySessionPolicy(request, body, identity)
			require.Equal(test, !existing, apiRelaySessionExempt(request))
			filter := applyAffinityGroupRouting(request, resolved, nil)
			require.Equal(test, existing, filter(codex))
			require.Equal(test, !existing, filter(relay))
		})
	}
}

func TestChatGroupRoutingReloadAndStorageFailure(test *testing.T) {
	path := filepath.Join(test.TempDir(), "routing.db")
	db, err := database.New("sqlite", path)
	require.NoError(test, err)
	key := sessionAffinityKey(continuityTestThread, 7)
	_, err = db.CommitSessionContinuity(test.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: 1})
	require.NoError(test, err)
	require.NoError(test, db.Close())
	db, err = database.New("sqlite", path)
	require.NoError(test, err)
	store := auth.NewStore(nil, nil, nil)
	test.Cleanup(store.Stop)
	primary := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
	split := &auth.Account{DBID: 2, GroupIDs: []int64{20}}
	store.AddAccount(primary)
	store.AddAccount(split)
	handler := &Handler{db: db, store: store}
	request, body, identity := chatGroupTestRequest(test, handler, "/v1/chat/completions")
	resolved := handler.configureAPIRelaySessionPolicy(request, body, identity)
	require.True(test, applyAffinityGroupRouting(request, resolved, nil)(primary))
	require.NoError(test, db.Close())
	request, body, identity = chatGroupTestRequest(test, handler, "/v1/chat/completions")
	resolved = handler.configureAPIRelaySessionPolicy(request, body, identity)
	filter := applyAffinityGroupRouting(request, resolved, nil)
	require.False(test, filter(primary))
	require.False(test, filter(split))
	require.Equal(test, "group_routing_owner_lookup_failed", usageRequestDiagnosticState(request).GroupRouting.Reason)
}

func TestChatGroupRoutingDisabledAndExactPath(test *testing.T) {
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true", nil)
	require.True(test, chatCompletionsGroupRouting(request))
	request.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7})
	inner := func(account *auth.Account) bool { return account != nil && account.ID() == 1 }
	filter := applyAffinityGroupRouting(request, requestSessionIdentity{hasRequestFingerprint: true}, inner)
	require.True(test, filter(&auth.Account{DBID: 1}))
	require.False(test, filter(&auth.Account{DBID: 2}))
	request.Request.URL.Path = "/v1/responses"
	request.Request.Header.Set("X-Original-URL", "/v1/chat/completions")
	require.False(test, chatCompletionsGroupRouting(request))
}

func TestChatGroupRoutingActualIngressKeepsNewAndExistingSessions(test *testing.T) {
	for _, existing := range []bool{false, true} {
		test.Run(map[bool]string{false: "new_split_session", true: "existing_primary_session"}[existing], func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			keyID, err := handler.db.InsertAPIKey(test.Context(), "chat-routing", "test-key")
			require.NoError(test, err)
			handler.store.SetAPIKeyAllowedGroups(keyID, []int64{10})
			handler.store.SetAPIKeyNoAffinityGroups(keyID, []int64{20})
			previousResin, previousRuntime := GetResinConfig(), CurrentRuntimeSettings()
			test.Cleanup(func() { SetResinConfig(previousResin); ApplyRuntimeSettings(previousRuntime) })
			ApplyRuntimeSettings(DefaultRuntimeSettings())
			test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			primary := &auth.Account{DBID: 1, GroupIDs: []int64{10}, AccountID: accountIdentitySampleAccount, AccessToken: "primary-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			split := &auth.Account{DBID: 2, GroupIDs: []int64{20}, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "split-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			handler.store.AddAccount(primary)
			handler.store.AddAccount(split)
			if existing {
				_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(sessionAffinityKey(continuityTestThread, keyID)), database.SessionContinuityRecord{AccountID: primary.ID(), ThreadID: continuityTestThread, NumberKnown: true})
				require.NoError(test, err)
			}
			seen := make(chan string, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				seen <- request.Header.Get("Chatgpt-Account-Id")
				stickyFailureSuccess(writer)
			}))
			test.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "chat-group-test"})
			for round := range 2 {
				request, _, _ := chatGroupTestRequest(test, handler, "/v1/chat/completions")
				request.Set(contextAPIKeyID, keyID)
				apiKeyRowFromContext(request).ID = keyID
				body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"stream":true,"reasoning_effort":"low"}`)
				request.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				request.Request.Header.Set("Authorization", "Bearer test-key")
				request.Request.Header.Set("Content-Type", "application/json")
				request.Request.Header.Set("Session-Id", continuityTestThread)
				request.Request.Header.Set("Thread-Id", continuityTestThread)
				request.Request.Header.Set("X-Codex-Window-Id", continuityTestThread+":0")
				handler.ChatCompletions(request)
				require.Equal(test, http.StatusOK, request.Writer.Status())
				require.Len(test, seen, 1)
				expected := split
				if existing {
					expected = primary
				}
				require.Equal(test, expected.AccountID, <-seen)
				reason := "existing_session_binding"
				if !existing && round == 0 {
					reason = "chat_completions_path"
				}
				require.Equal(test, reason, usageRequestDiagnosticState(request).GroupRouting.Reason)
			}
		})
	}
}
