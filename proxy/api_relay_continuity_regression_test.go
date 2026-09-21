package proxy

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAPIRelayCannotDetachExistingNativeParentByModelList(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(fmt.Sprint(persisted), func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			native := &auth.Account{DBID: 1711, AccessToken: "native-test", Status: auth.StatusReady, Models: []string{"gpt-6-astra"}}
			relay := apiRelayPolicyTestAccount()
			h.store.AddAccounts([]*auth.Account{native, relay})
			request, body := continuityTestRequest(0, "turn")
			usageRequestDiagnosticState(request).StartedAt = time.Now()
			request.Set(contextAPIKeyID, int64(7))
			identity := requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true, hasRequestFingerprint: true, relatedToRoot: true, requiresRootAccount: true}
			key := sessionAffinityKey(identity.affinityID, 7)
			h.store.BindSessionAffinity(key, native, "")
			// A stale API shadow binding must never win over the native parent.
			h.store.BindSessionAffinity(sessionAffinityKey("api-relay:"+identity.affinityID, 7), relay, "")
			if persisted {
				_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: native.ID(), ThreadID: continuityTestThread})
				require.NoError(t, err)
			}
			resolved := h.configureAPIRelaySessionPolicy(request, body, identity)
			require.Equal(t, identity, resolved)
			require.False(t, apiRelaySessionExempt(request))
			require.Nil(t, h.waitForBackgroundRootAccount(request, resolved))
			filter := h.applyPassiveInternalModelRouting(request, "gpt-5.6-sol", resolved, capacityAwareSessionAffinityKey(resolved, 7), true, nil)
			require.True(t, filter(native))
			require.False(t, filter(relay))
			require.EqualValues(t, 1711, usageRequestDiagnosticState(request).RootAccountID)
		})
	}
}

func TestAPIRelayMigratesLegacyBindingWithoutChangingParent(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		t.Run(fmt.Sprint(bypass), func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			relay := apiRelayPolicyTestAccount()
			relay.SessionCapacityEnabled, relay.SessionCapacityMax = true, 1
			h.store.AddAccount(relay)
			request, body := continuityTestRequest(0, "turn")
			usageRequestDiagnosticState(request).StartedAt = time.Now()
			request.Set(contextAPIKeyID, int64(7))
			legacy := sessionAffinityKey("api-relay:"+continuityTestThread, 7)
			if bypass {
				legacy = auth.SessionAccountingBypassAffinityKey(legacy)
			}
			h.store.BindSessionAffinity(legacy, relay, "")
			identity := h.configureAPIRelaySessionPolicy(request, body, requestSessionIdentity{affinityID: continuityTestThread, stableIdentity: true, relatedToRoot: true, requiresRootAccount: true})
			require.Equal(t, continuityTestThread, identity.affinityID)
			require.True(t, identity.relatedToRoot)
			require.Nil(t, h.waitForBackgroundRootAccount(request, identity))
			owner, found := h.store.LiveSessionAccountID(sessionAffinityKey(continuityTestThread, 7), time.Now())
			require.True(t, found)
			require.Equal(t, relay.ID(), owner)
			total, _ := h.store.AccountSessionSlotCounts(relay.ID(), time.Now())
			require.EqualValues(t, 1, total, "migration reuses the old slot even when the account is full")
		})
	}
}

func TestAPIRelayOptionalHeadersStillDetectBodyIdentityConflicts(t *testing.T) {
	for _, sample := range []struct{ body, want string }{
		{`{}`, "not_applicable"},
		{`{"client_metadata":{"session_id":"root","x-codex-turn-metadata":{"session_id":"root"}}}`, "body_only"},
		{`{"client_metadata":{"session_id":"root","x-codex-turn-metadata":{"session_id":"other"}}}`, "mismatched"},
	} {
		identity := &outboundIdentityDiagnostic{SessionHeaderPolicy: "optional", HTTP: CaptureOutboundIdentityHeaders(http.Header{}), Body: captureOutboundIdentityBody([]byte(sample.body))}
		require.Equal(t, sample.want, outboundSessionConsistency(identity))
	}
}

func TestAPIRelayKeepsLegacyProtocolAliasesWithUnifiedRouting(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	relay := apiRelayPolicyTestAccount()
	h.store.AddAccount(relay)
	root, turn := NewUpstreamSessionUUID(), NewUpstreamSessionUUID()
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":%q,"thread_id":%q,"turn_id":%q,"thread_source":"user"}}}`, root, root, turn))
	makeRequest := func(token string) *gin.Context {
		request, _ := gin.CreateTestContext(httptest.NewRecorder())
		request.Set(contextAPIKeyID, int64(7))
		request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		request.Request.Header.Set(codexSessionIDHeader, root)
		request.Request.Header.Set(codexTurnStateHeader, token)
		return request
	}
	old := makeRequest("")
	oldIdentity := requestSessionIdentity{affinityID: "api-relay:" + root, stableIdentity: true}
	h.bindTurnStateSession(old, body, oldIdentity)
	h.bindResponseIdentity(old, oldIdentity)
	real := "legacy-relay-turn-state"
	alias, err := turnStateSessionFrom(old.Request.Context()).issue(old.Request.Context(), relay, real, "response_header")
	require.NoError(t, err)
	_, oldBinding := protocolIdentityBinding(old.Request.Context(), relay)
	h.store.BindSessionAffinity(sessionAffinityKey(oldIdentity.affinityID, 7), relay, "")
	current := makeRequest(alias)
	identity := h.configureAPIRelaySessionPolicy(current, body, requestSessionIdentity{affinityID: root, stableIdentity: true})
	h.bindTurnStateSession(current, body, identity)
	h.bindResponseIdentity(current, identity)
	_, currentBinding := protocolIdentityBinding(current.Request.Context(), relay)
	require.Equal(t, oldBinding, currentBinding, "history metadata retains its authenticated alias namespace")
	require.Equal(t, root, identity.affinityID, "selection uses the original shared root")
	normalized := normalizeTurnStateIngress(current, body)
	_, headers := PrepareCodexTurnStateOutbound(current.Request.Context(), relay, normalized, current.Request.Header)
	require.Equal(t, real, headers.Get(codexTurnStateHeader))
	other := apiRelayPolicyTestAccount()
	other.DBID++
	h.store.AddAccount(other)
	h.store.BindSessionAffinity(sessionAffinityKey(root, 7), other, "")
	changed := makeRequest(alias)
	identity = h.configureAPIRelaySessionPolicy(changed, body, requestSessionIdentity{affinityID: root, stableIdentity: true})
	h.bindTurnStateSession(changed, body, identity)
	normalizeTurnStateIngress(changed, body)
	require.Empty(t, changed.Request.Header.Get(codexTurnStateHeader), "legacy compatibility cannot authorize a token from another account")
}

func TestUsageTurnStateResponseTraceIgnoresLateAttempt(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	request, _ := continuityTestRequest(0, "turn")
	attachUpstreamTrace(request, h.store)
	account := apiRelayPolicyTestAccount()
	beginUsageSelectionAttempt(request, 1)
	old := request.Request.Context()
	beginUpstreamTrace(old, account, "", false)
	observeUsageTurnState(old, "")
	beginUsageSelectionAttempt(request, 2)
	beginUpstreamTrace(request.Request.Context(), account, "", false)
	observeUsageTurnState(old, "late-state-from-previous-attempt")
	require.Nil(t, snapshotUpstreamTrace(request.Request.Context()).Transport.ResponseTurnState)
	observeUsageTurnState(request.Request.Context(), "current")
	require.Equal(t, len("current"), snapshotUpstreamTrace(request.Request.Context()).Transport.ResponseTurnState.Length)
}

func TestAPIRelayTurnStateAndDiagnosticsAcrossRequests(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	config := h.store.GetPromptFilterConfig()
	config.Enabled, config.Advanced.NewAPI.Enabled = false, false
	h.store.SetPromptFilterConfig(config)
	real := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, 217))
	var sentHeaders http.Header
	var sentBody []byte
	var step int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentBody, _ = io.ReadAll(r.Body)
		sentHeaders = r.Header.Clone()
		if step == 0 || step == 3 {
			w.Header().Set(codexTurnStateHeader, real)
		}
		if step == 3 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"resp_relay_compact","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if step == 2 {
			fmt.Fprintf(w, "data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":%q}}\n\n", real)
		}
		fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_relay_%d\"}}\n\n", step)
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_relay_%d\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n", step)
	}))
	t.Cleanup(server.Close)
	relay := apiRelayPolicyTestAccount()
	relay.BaseURL = server.URL
	relay.SessionCapacityEnabled, relay.SessionCapacityMax = true, 1
	h.store.AddAccount(relay)
	root, turn := NewUpstreamSessionUUID(), NewUpstreamSessionUUID()
	var alias string
	for step = 0; step < 5; step++ {
		path := "/v1/responses"
		if step == 3 {
			path += "/compact"
		}
		if step == 2 || step == 4 {
			turn = NewUpstreamSessionUUID()
			if step == 2 {
				alias = ""
			} // Step 4 deliberately echoes the previous turn's alias.
		}
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"hello","stream":true,"prompt_cache_key":%q,"client_metadata":{"session_id":%q,"thread_id":%q,"x-codex-turn-metadata":{"session_id":%q,"thread_id":%q,"turn_id":%q,"thread_source":"user","request_kind":"turn"}}}`, root, root, root, root, root, turn))
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Set(contextAPIKeyID, int64(7))
		request.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Request.Header.Set(codexSessionIDHeader, root)
		request.Request.Header.Set(codexTurnStateHeader, alias)
		attachUpstreamTrace(request, h.store)
		if step == 3 {
			h.ResponsesCompact(request)
		} else {
			h.Responses(request)
		}
		require.Equal(t, http.StatusOK, recorder.Code, "step=%d %s", step, recorder.Body.String())
		if step == 1 || step == 3 {
			require.Equal(t, real, sentHeaders.Get(codexTurnStateHeader), "continuation step=%d", step)
		} else {
			require.Empty(t, sentHeaders.Get(codexTurnStateHeader), "new turn step=%d", step)
		}
		require.NotContains(t, string(sentBody), "account_mapping")
		require.NotContains(t, string(sentBody), "format_version")
		usage := &database.UsageLogInput{AccountID: relay.ID()}
		populateUpstreamTrace(request, usage)
		populateUsageRequestDiagnostics(request, usage)
		require.Equal(t, "mapped", gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.account_mapping.status").String())
		require.True(t, gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.account_mapping.changes").IsArray())
		consistency := "body_only"
		if step == 3 {
			consistency = "not_applicable"
		} // The compact wire schema omits client metadata.
		require.Equal(t, consistency, gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.session_consistency").String())
		// Inspect the actual saved attempt, before the handler restores its
		// outer request context while unwinding keepalive/cancellation scopes.
		var saved database.UsageLog
		require.Eventually(t, func() bool {
			h.db.FlushUsageLogs()
			logs, err := h.db.ListRecentUsageLogs(t.Context(), 10)
			if err != nil || len(logs) < step+1 {
				return false
			}
			for _, item := range logs {
				if item.RequestID == usage.RequestID {
					saved = *item
					return true
				}
			}
			return false
		}, 3*time.Second, 10*time.Millisecond)
		require.NotNil(t, saved.TurnStateLength)
		if step == 4 {
			require.Zero(t, *saved.TurnStateLength)
		} else {
			require.Equal(t, len(real), *saved.TurnStateLength, "step=%d", step)
		}
		if step == 0 || step == 2 || step == 3 {
			alias = recorder.Header().Get(codexTurnStateHeader)
			require.NotEmpty(t, alias, "step=%d", step)
			require.NotEqual(t, real, alias)
			require.NotContains(t, recorder.Body.String(), real)
		}
		owner, found := h.store.LiveSessionAccountID(sessionAffinityKey(root, 7), time.Now())
		require.True(t, found)
		require.Equal(t, relay.ID(), owner)
	}
}

func TestAPIRelaySignedBackgroundSharesMainAccountAndWindow(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	var path, authorization string
	var sent []byte
	server := newOpenAIResponsesSSEUpstream(&path, &authorization, &sent)
	t.Cleanup(server.Close)
	relay := apiRelayPolicyTestAccount()
	relay.BaseURL = server.URL
	relay.SessionCapacityEnabled, relay.SessionCapacityMax = true, 1
	h.store.AddAccount(relay)
	root := NewUpstreamSessionUUID()
	fingerprint := newAPIRootSessionFingerprint("test-platform", "42", root)
	for _, source := range []string{"user", "subagent", "guardian_review", "guardian_classifier", "thread_title"} {
		t.Run(source, func(t *testing.T) {
			relation, feature := newAPIPolicyRootSessionRelationRoot, ""
			thread := root
			if source != "user" {
				relation, feature = newAPIPolicyRootSessionRelationRelated, newAPIPassiveFeatureRelatedInternal
				thread = NewUpstreamSessionUUID()
			}
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: relation, RootSessionFingerprint: fingerprint, RootSessionID: root, ThreadSource: source, RequestKind: "turn", PassiveFeature: feature}
			wait := int64(1500)
			meta.RootAccountWaitMillis = &wait
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"hello","stream":true,"client_metadata":{"x-codex-turn-metadata":{"session_id":%q,"thread_id":%q,"parent_thread_id":%q,"thread_source":%q,"request_kind":"turn"}}}`, root, thread, root, source))
			request, recorder := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, meta)
			attachUpstreamTrace(request, h.store)
			h.Responses(request)
			require.Equal(t, http.StatusOK, recorder.Code, "%s: %s", source, recorder.Body.String())
			usage := &database.UsageLogInput{AccountID: relay.ID()}
			populateUpstreamTrace(request, usage)
			populateUsageRequestDiagnostics(request, usage)
			if source != "user" {
				require.Equal(t, "related_internal", usage.RequestType, source)
				require.Equal(t, "found", usageRequestDiagnosticState(request).RootAccountLookup)
				require.Equal(t, relay.ID(), usageRequestDiagnosticState(request).RootAccountID)
				require.Equal(t, "matched", usageRequestDiagnosticState(request).BackgroundAccountMatch.Result)
			}
			total, _ := h.store.AccountSessionSlotCounts(relay.ID(), time.Now())
			require.EqualValues(t, 1, total, source)
		})
	}
}
