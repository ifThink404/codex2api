package proxy

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestForkContinuityRestoresOriginalParentBeforeNonzeroValidation(test *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, durable := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/durable=%t", transport, durable), func(test *testing.T) {
				handler := newWindowAuthorizationHandler(test)
				config := handler.store.GetPromptFilterConfig()
				config.Advanced.Risk.SessionContinuityMode = "enforce"
				handler.store.SetPromptFilterConfig(config)
				owner := &auth.Account{DBID: 1695, Status: auth.StatusReady, AccessToken: "test", Models: []string{"gpt-5.6-sol"}}
				handler.store.AddAccount(owner)
				request, body := continuityTestRequest(27, "turn")
				if transport == "websocket" {
					request.Request.Method = http.MethodGet
					request.Request.Header.Set("Connection", "Upgrade")
					request.Request.Header.Set("Upgrade", "websocket")
				}
				sourceKey := sessionAffinityKey(accountIdentitySampleRoot, requestAPIKeyID(request))
				targetKey := sessionAffinityKey(continuityTestThread, requestAPIKeyID(request))
				if durable {
					_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(sourceKey), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: accountIdentitySampleRoot, Number: 47, NumberKnown: true, LastSeen: time.Now()})
					require.NoError(test, err)
				} else {
					handler.store.BindSessionAffinity(sourceKey, owner, "")
				}
				identity := requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: accountIdentitySampleRoot}
				require.Nil(test, handler.configureSessionModelAffinity(request, identity, targetKey, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				require.Equal(test, owner.ID(), selectionTraceForRequest(request).PinnedAccount())
				require.Equal(test, "baseline", usageRequestDiagnosticState(request).Continuity.Result)
				require.Equal(test, uint64(27), continuityRequest(request).Number)
				require.Zero(test, continuityRequest(request).Record.AccountID)
				require.Nil(test, handler.commitSessionContinuity(request, owner))
				record, found, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(targetKey))
				require.NoError(test, err)
				require.True(test, found)
				require.Equal(test, continuityTestThread, record.ThreadID)
				require.Equal(test, uint64(27), record.Number)
				if durable {
					parent, found, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(sourceKey))
					require.NoError(test, err)
					require.True(test, found)
					require.Equal(test, uint64(47), parent.Number)
				}
			})
		}
	}
}

func TestForkContinuityDoesNotGuessOrBorrowOtherUserOwner(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	request, body := continuityTestRequest(27, "turn")
	otherSource := "newapi-root-session:" + newAPIRootSessionFingerprint("newapi", "other", accountIdentitySampleRoot)
	wantedSource := "newapi-root-session:" + newAPIRootSessionFingerprint("newapi", "current", accountIdentitySampleRoot)
	_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(sessionAffinityKey(otherSource, 0)), database.SessionContinuityRecord{AccountID: 1695, ThreadID: accountIdentitySampleRoot, NumberKnown: true})
	require.NoError(test, err)
	failure := handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: wantedSource}, sessionAffinityKey(continuityTestThread, 0), body)
	require.NotNil(test, failure)
	require.Equal(test, "codex_session_continuity_fork_owner_unavailable", string(failure.Code))
	require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(failure.Code))
	require.Zero(test, selectionTraceForRequest(request).PinnedAccount())
}

func TestTakeForkSourceAccountUsesDurableOwnerRatherThanStaleLiveBinding(test *testing.T) {
	handler, old, current, _ := failoverTestSetup(test, true)
	sourceKey := sessionAffinityKey(accountIdentitySampleRoot, 0)
	targetKey := sessionAffinityKey(continuityTestThread, 0)
	handler.store.BindSessionAffinity(sourceKey, old, "")
	_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(sourceKey), database.SessionContinuityRecord{AccountID: current.ID(), ThreadID: accountIdentitySampleRoot, NumberKnown: true})
	require.NoError(test, err)
	selected, _ := handler.takeForkSourceAccount(test.Context(), requestSessionIdentity{forkSourceAffinityID: accountIdentitySampleRoot}, targetKey, 0, nil, nil, auth.DispatchPolicyStandard)
	require.Same(test, current, selected)
	handler.store.Release(selected)
}

func TestForkCompactionKeepsExistingChildOrSelectsFirstOwner(test *testing.T) {
	for _, existing := range []bool{false, true} {
		test.Run(fmt.Sprint(existing), func(test *testing.T) {
			handler, parent, child, _ := failoverTestSetup(test, true)
			request, body := continuityTestRequest(27, "compaction")
			sourceKey := sessionAffinityKey(accountIdentitySampleRoot, 0)
			targetKey := sessionAffinityKey(continuityTestThread, 0)
			_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(sourceKey), database.SessionContinuityRecord{AccountID: parent.ID(), ThreadID: accountIdentitySampleRoot, NumberKnown: true})
			require.NoError(test, err)
			if existing {
				_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(targetKey), database.SessionContinuityRecord{AccountID: child.ID(), ThreadID: continuityTestThread, Number: 27, NumberKnown: true})
				require.NoError(test, err)
			}
			failure := handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: accountIdentitySampleRoot}, targetKey, body)
			if existing {
				require.Nil(test, failure)
				require.Equal(test, child.ID(), selectionTraceForRequest(request).PinnedAccount())
			} else {
				require.Nil(test, failure)
				require.Zero(test, selectionTraceForRequest(request).PinnedAccount())
				require.True(test, continuityRequest(request).InitialCompaction)
			}
		})
	}
}
