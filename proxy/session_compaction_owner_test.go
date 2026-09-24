package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSessionContinuityCompactionAllowsFirstOwner(test *testing.T) {
	for _, mode := range []string{"off", "observe", "enforce"} {
		for _, scenario := range []string{"metadata", "nonzero", "missing_window", "missing_source", "endpoint", "protocol", "header", "body_metadata"} {
			test.Run(mode+"/"+scenario, func(test *testing.T) {
				handler := newRootlessPassiveModelTestHandler(test)
				config := handler.store.GetPromptFilterConfig()
				config.Advanced.Risk.SessionContinuityMode = mode
				handler.store.SetPromptFilterConfig(config)
				request, body := continuityTestRequest(0, "compaction")
				resolved := usageRequestDiagnosticState(request).Resolved
				switch scenario {
				case "nonzero":
					request, body = continuityTestRequest(71, "compaction")
				case "missing_window":
					body = []byte(`{"model":"gpt-5.6-sol","input":"compact"}`)
				case "missing_source":
					resolved.ThreadSource, resolved.RootState = "", "resolved"
				case "endpoint", "protocol", "header":
					resolved.RequestKind = "turn"
					body = bytes.ReplaceAll(body, []byte(`"compaction"`), []byte(`"turn"`))
					if scenario == "endpoint" {
						request.Request.URL.Path = "/v1/responses/compact"
					} else if scenario == "protocol" {
						body = bytes.Replace(body, []byte(`"model":`), []byte(`"input":[{"type":"compaction_trigger"}],"model":`), 1)
					} else {
						request.Request.Header.Set(codexTurnMetadataHeader, `{"request_kind":"compaction"}`)
						body = []byte(`{"model":"gpt-5.6-sol"}`)
					}
				case "body_metadata":
					resolved.RequestKind = "turn"
				}
				key := "unbound-compaction::api-key:101"
				apiErr := handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true}, key, body)
				require.Nil(test, apiErr)
				diagnostic := usageRequestDiagnosticState(request).Continuity
				require.NotEqual(test, "blocked", diagnostic.Action)
				require.False(test, diagnostic.WouldBlock)
				require.Equal(test, "missing", diagnostic.OwnerSource)
				require.NotNil(test, continuityRequest(request))
				require.True(test, continuityRequest(request).InitialCompaction)
				require.Zero(test, selectionTraceForRequest(request).PinnedAccount())
				_, found := handler.store.LiveSessionAccountID(key, time.Now())
				require.False(test, found)
				require.Empty(test, handler.continuityRecords)
			})
		}
	}
}

func TestSessionContinuityCompactionRecoversOwnerWithoutSwitching(test *testing.T) {
	for _, scenario := range []string{"live", "persistent", "paused", "disabled", "quota", "model_unavailable"} {
		test.Run(scenario, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			owner := &auth.Account{DBID: 1589, AccessToken: "owner-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			other := &auth.Account{DBID: 1707, AccessToken: "other-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			handler.store.AddAccounts([]*auth.Account{owner, other})
			switch scenario {
			case "paused":
				atomic.StoreInt32(&owner.DispatchPaused, 1)
			case "disabled":
				atomic.StoreInt32(&owner.Disabled, 1)
			case "quota":
				owner.PlanType, owner.UsagePercent7d, owner.UsagePercent7dValid = "free", 100, true
				owner.Reset7dAt = time.Now().Add(time.Hour)
			case "model_unavailable":
				owner.Models = []string{"gpt-5.6-terra"}
			}
			key := "original-root::api-key:101"
			if scenario == "live" {
				handler.store.BindSessionAffinity(key, owner, "")
			} else {
				_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{
					AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now().Add(-21 * time.Hour),
				})
				require.NoError(test, err)
			}
			for _, mode := range []string{"off", "observe", "enforce"} {
				config := handler.store.GetPromptFilterConfig()
				config.Advanced.Risk.SessionContinuityMode = mode
				handler.store.SetPromptFilterConfig(config)
				request, body := continuityTestRequest(0, "compaction")
				apiErr := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
				if scenario == "model_unavailable" {
					require.NotNil(test, apiErr)
					require.Equal(test, api.ErrCodeSessionModelUnavailable, apiErr.Code)
				} else {
					require.Nil(test, apiErr)
				}
				trace := selectionTraceForRequest(request)
				require.Equal(test, owner.ID(), trace.PinnedAccount())
				selected, _, _ := handler.store.NextForSessionWithDispatchGuard(key, 101, nil, nil, auth.DispatchPolicyStandard, trace)
				if scenario == "live" || scenario == "persistent" {
					require.Same(test, owner, selected)
					handler.store.Release(selected)
				} else {
					require.Nil(test, selected)
				}
				require.Nil(test, handler.store.TakePreferredAccountWithDispatch(other.ID(), 101, nil, nil, auth.DispatchPolicyStandard, trace))
			}
		})
	}
}

func TestSessionContinuityCompactionDoesNotBorrowDifferentBinding(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	owner := &auth.Account{DBID: 1589, AccessToken: "owner-test", Status: auth.StatusReady}
	handler.store.AddAccount(owner)
	for _, key := range []string{"same-root::api-key:102", "other-root::api-key:101"} {
		handler.store.BindSessionAffinity(key, owner, "")
		_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), LastSeen: time.Now()})
		require.NoError(test, err)
	}
	request, body := continuityTestRequest(0, "compaction")
	key := "same-root::api-key:101"
	apiErr := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
	require.Nil(test, apiErr)
	require.NotNil(test, continuityRequest(request))
	require.Zero(test, selectionTraceForRequest(request).PinnedAccount())
	_, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.False(test, found)
}

func TestSessionContinuityCompactionIgnoresHistoryAndStaleWebSocketMetadata(test *testing.T) {
	for _, scenario := range []string{"ordinary", "history", "tool_output", "websocket_handshake"} {
		test.Run(scenario, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			request, body := continuityTestRequest(0, "turn")
			switch scenario {
			case "history":
				body = bytes.Replace(body, []byte(`"model":`), []byte(`"input":[{"type":"compaction","encrypted_content":"history"}],"model":`), 1)
			case "tool_output":
				body = bytes.Replace(body, []byte(`"model":`), []byte(`"input":[{"type":"function_call_output","call_id":"call_test","output":{"type":"compaction_trigger"}}],"model":`), 1)
			case "websocket_handshake":
				usageRequestDiagnosticState(request).Resolved.RequestKind = "compaction"
				request.Request.Header.Set("Connection", "Upgrade")
				request.Request.Header.Set("Upgrade", "websocket")
				request.Request.Header.Set(codexTurnMetadataHeader, `{"request_kind":"compaction"}`)
				cacheRequestCompactionMeta(request, requestCompactionMeta{UsageTriggered: true})
			}
			require.False(test, requestIsSessionCompaction(request, body))
			require.Nil(test, handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true}, "fresh-root::api-key:101", body))
			require.Equal(test, "new_root", usageRequestDiagnosticState(request).Continuity.Result)
			require.NotNil(test, continuityRequest(request))
		})
	}
}

func TestSessionContinuityUnboundCompactionReachesHTTPOrWebSocketUpstream(test *testing.T) {
	for _, mode := range []string{"off", "observe", "enforce"} {
		for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "websocket"} {
			test.Run(mode+"/"+endpoint, func(test *testing.T) {
				var upstreamCalls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					upstreamCalls.Add(1)
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"id":"resp_unexpected","status":"completed","output":[]}`))
				}))
				defer upstream.Close()
				handler := newRootlessPassiveModelTestHandler(test)
				account := &auth.Account{DBID: 1707, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
				useCodexHTTPTestAccounts(test, account)
				handler.store.AddAccount(account)
				config := handler.store.GetPromptFilterConfig()
				config.Advanced.Risk.SessionContinuityMode = mode
				handler.store.SetPromptFilterConfig(config)
				_, body := continuityTestRequest(0, "compaction")
				body = bytes.Replace(body, []byte(`"model":`), []byte(`"type":"response.create","input":"compact","model":`), 1)
				meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: newAPIRootSessionFingerprint("test-platform", "42", continuityTestThread), ThreadSource: "user", RequestKind: "compaction"}
				if endpoint != "websocket" {
					request, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, endpoint, body, meta)
					if endpoint == "/v1/responses" {
						handler.Responses(request)
					} else {
						handler.ResponsesCompact(request)
					}
					require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
					require.Empty(test, gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
					require.NotEqual(test, "blocked", usageRequestDiagnosticState(request).Continuity.Action)
				} else {
					router := gin.New()
					router.GET("/v1/responses", func(request *gin.Context) {
						request.Set(contextAPIKeyID, int64(101))
						handler.ResponsesWebSocket(request)
					})
					server := httptest.NewServer(router)
					defer server.Close()
					meta.RequestKind = "turn"
					request, _ := signedRootlessPassiveModelContext(test, http.MethodGet, "/v1/responses", nil, meta)
					connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", request.Request.Header)
					require.NoError(test, err)
					defer connection.Close()
					require.NoError(test, connection.WriteMessage(websocket.TextMessage, body))
					require.NoError(test, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
					_, response, err := connection.ReadMessage()
					require.NoError(test, err)
					require.Equal(test, "response.completed", gjson.GetBytes(response, "type").String(), string(response))
				}
				require.Equal(test, int32(1), upstreamCalls.Load())
			})
		}
	}
}
