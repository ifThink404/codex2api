package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestWindowCapacityQuoteReachesAccountFailover(test *testing.T) {
	for _, scenario := range []string{"success", "relaxed", "disabled", "different_tags", "different_groups", "no_capacity", "wrong_model", "opaque_only", "user_limit", "expansion_priority", "expansion_no_reserved", "expansion_quota_exhausted"} {
		test.Run(scenario, func(test *testing.T) {
			handler, owner, target, _ := failoverTestSetup(test, scenario != "disabled")
			if scenario == "relaxed" {
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
					s.CodexSessionFailoverEnabled, s.CodexForkAccountFallbackEnabled = false, true
					return s
				})
			}
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Risk.SessionCreationLimit = 5
			config.Advanced.Risk.SessionCreationLimitWindowSeconds = 10800
			handler.store.SetPromptFilterConfig(config)
			initial := quoteWindowAuthorization(test, handler, "capacity-root", "initial", "user")
			oldRoot := initial.Grant.Root
			initial.Fingerprint = newAPIRootSessionFingerprint(initial.Platform, initial.UserID, continuityTestThread)
			initial.Grant.Root = hashRiskIdentity(initial.Fingerprint)
			key := sessionAffinityKey("newapi-root-session:"+initial.Fingerprint, initial.APIKeyID)
			subject := cache.PromptSessionLimitSubject(initial.Platform, initial.UserID)
			now := time.Now().UTC()
			owner.SessionCapacityEnabled, owner.SessionCapacityMax, owner.SessionCapacityReserved, owner.SessionCapacityIdleTTLSeconds = true, 5, 2, 1800
			target.SessionCapacityEnabled, target.SessionCapacityMax = true, 5
			for _, other := range []string{"other-1", "other-2", "other-3"} {
				require.True(test, handler.store.AdmitAccountSession(owner, other, now))
			}
			for _, account := range []*auth.Account{owner, target} {
				require.True(test, handler.store.ApplyAccountGroups(account.ID(), []int64{11, 22}))
				require.True(test, handler.store.ApplyAccountTags(account.ID(), []string{"pool-a", "pro"}))
			}
			require.NoError(test, handler.db.UpdateUserWindowAdmissions(test.Context(), subject, func(state *database.UserWindowAdmissionState) error {
				delete(state.Windows, oldRoot)
				delete(state.Reservations, oldRoot)
				return nil
			}))
			_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, Number: 47, NumberKnown: true, LastSeen: now.Add(-14 * time.Hour)})
			require.NoError(test, err)
			quoteWindowAuthorization(test, handler, "other-user-window-1", "one", "user")
			quoteWindowAuthorization(test, handler, "other-user-window-2", "two", "user")
			if scenario == "expansion_no_reserved" {
				trace := &auth.SelectionTrace{}
				trace.SetExpandedWindow(true)
				for _, occupied := range []string{"reserved-one", "reserved-two"} {
					require.True(test, handler.store.AdmitAccountSession(owner, occupied, now, trace))
				}
			}
			if scenario == "expansion_quota_exhausted" {
				require.NoError(test, handler.db.UpdateUserWindowAdmissions(test.Context(), subject, func(state *database.UserWindowAdmissionState) error {
					state.Windows["other-expanded"] = &database.UserWindowGrant{ID: "expanded", Root: "other-expanded", Expanded: true, Confirmed: true, Multiplier: 1.1, ExpiresAt: now.Add(time.Hour)}
					return nil
				}))
			}
			if scenario == "user_limit" {
				config.Advanced.Risk.SessionCreationLimit = 2
				handler.store.SetPromptFilterConfig(config)
			}
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: initial.Fingerprint, ThreadSource: "user", RequestKind: "turn"}
			quoteBody := []byte(`{"operation":"quote_tiered","allow_expansion":false,"extra_limit":5,"multiplier":1.5,"multiplier_step":0.1,"reservation_id":"current"}`)
			if strings.HasPrefix(scenario, "expansion_") {
				quoteBody, _ = sjson.SetBytes(quoteBody, "allow_expansion", true)
			}
			if scenario == "expansion_quota_exhausted" {
				quoteBody, _ = sjson.SetBytes(quoteBody, "extra_limit", 1)
			}
			quote, quoteResponse := windowExpansionTestContext(test, "/v1/session-windows", quoteBody, meta)
			handler.ControlNewAPIUserWindows(quote)
			diagnostic := windowControlDiagnostic(quote)
			require.Equal(test, "session_capacity_full", diagnostic.Account.Reason)
			if scenario == "disabled" || scenario == "user_limit" {
				require.Equal(test, http.StatusBadRequest, quoteResponse.Code)
				require.Equal(test, scenario != "disabled", diagnostic.CapacityFailoverDeferred)
				return
			}
			require.Equal(test, http.StatusOK, quoteResponse.Code, quoteResponse.Body.String())
			preferExpansion := scenario == "expansion_priority"
			require.Equal(test, !preferExpansion, diagnostic.CapacityFailoverDeferred)
			require.Equal(test, preferExpansion, diagnostic.NeedsExpansion)
			require.Equal(test, 2, diagnostic.OrdinaryUsed)
			var result struct {
				Ticket string `json:"ticket"`
			}
			require.NoError(test, json.Unmarshal(quoteResponse.Body.Bytes(), &result))
			grant, err := decodeWindowGrant("integration-secret", result.Ticket)
			require.NoError(test, err)
			require.Equal(test, owner.ID(), grant.Grant.OwnerAccountID, "quote must not choose a new account")
			require.False(test, grant.Grant.Confirmed)
			require.Equal(test, preferExpansion, grant.Grant.Expanded)
			if preferExpansion {
				require.Equal(test, 1.1, grant.Grant.Multiplier)
			} else {
				require.Equal(test, 1.0, grant.Grant.Multiplier)
			}
			_, body := outboundEpochTestRequest(test, handler, 47)
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"role":"user","content":"keep this task"},{"type":"reasoning","encrypted_content":"old-encrypted"}]`))
			body, _ = sjson.SetBytes(body, "previous_response_id", "old-response")
			switch scenario {
			case "different_tags":
				require.True(test, handler.store.ApplyAccountTags(target.ID(), []string{"pool-b", "pro"}))
			case "different_groups":
				require.True(test, handler.store.ApplyAccountGroups(target.ID(), []int64{11}))
			case "no_capacity":
				target.SessionCapacityMax = 1
				require.True(test, handler.store.AdmitAccountSession(target, "occupied", now))
			case "wrong_model":
				target.Models = []string{"gpt-5.5"}
			case "opaque_only":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"old-only"}]`))
			}
			body = addSessionTools(test, body)
			meta.WindowGrant = result.Ticket
			request, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
			request.Set(ingressRequestBodyContextKey, body)
			handler.primeNewAPIPolicyContext(request, body)
			_, policy := handler.cachedNewAPIPolicyAuditState(request)
			bindTransportOwner(request, policy, true)
			handler.bindCodexIdentityClaims(request)
			beginDispatchSelection(request)
			require.Nil(test, handler.requestWindowGrantError(request))
			usageRequestDiagnosticState(request).Resolved = &usageRequestResolution{ThreadSource: "user", RequestKind: "turn", Stable: true}
			failure := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true, ownsRootBinding: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
			if scenario == "opaque_only" {
				require.NotNil(test, failure)
				stored, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
				require.NoError(test, err)
				require.Equal(test, owner.ID(), stored.AccountID)
				return
			}
			require.Nil(test, failure)
			selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, initial.APIKeyID, nil, sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false), auth.DispatchPolicyStandard)
			if preferExpansion {
				require.False(test, handled, "authorized expansion should keep the original account")
				require.Nil(test, selected)
				require.True(test, handler.store.AdmitAccountSession(owner, key, now, selectionTraceForRequest(request)))
				status, blocked := handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(request, body, owner, key, owner.ID())
				require.False(test, blocked, "%+v", status)
				updated, err := decodeWindowGrant("integration-secret", request.Writer.Header().Get(windowGrantResponseHeader))
				require.NoError(test, err)
				require.True(test, updated.Grant.Confirmed)
				require.True(test, updated.Grant.Expanded)
				require.Equal(test, 1.1, updated.Grant.Multiplier)
				require.Equal(test, owner.ID(), updated.Grant.OwnerAccountID)
				stored, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
				require.NoError(test, err)
				require.Zero(test, stored.FailoverCount)
				require.False(test, stored.LossyContextRestart)
				return
			}
			require.True(test, handled)
			if scenario != "success" && scenario != "relaxed" && scenario != "different_tags" && scenario != "expansion_no_reserved" && scenario != "expansion_quota_exhausted" {
				require.Nil(test, selected)
				require.Equal(test, "no_safe_candidate", usageRequestDiagnosticState(request).AccountFailover.Result)
				stored, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
				require.NoError(test, err)
				require.Equal(test, owner.ID(), stored.AccountID)
				admissions, err := handler.db.ReadUserWindowAdmissions(test.Context(), subject)
				require.NoError(test, err)
				require.False(test, admissions.Windows[grant.Grant.Root].Confirmed)
				require.Equal(test, owner.ID(), admissions.Windows[grant.Grant.Root].OwnerAccountID)
				return
			}
			require.Same(test, target, selected)
			defer handler.store.Release(selected)
			require.False(test, windowGrantForRequest(request).Grant.Confirmed, "migration must leave admission confirmation to dispatch")
			require.Equal(test, target.ID(), windowGrantForRequest(request).Grant.OwnerAccountID)
			status, blocked := handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(request, body, target, key, owner.ID())
			require.False(test, blocked, "%+v", status)
			updated, err := decodeWindowGrant("integration-secret", request.Writer.Header().Get(windowGrantResponseHeader))
			require.NoError(test, err)
			require.True(test, updated.Grant.Confirmed)
			require.Equal(test, target.ID(), updated.Grant.OwnerAccountID)
			require.Equal(test, grant.Grant.ID, updated.Grant.ID)
			require.Equal(test, 1.0, updated.Grant.Multiplier)
			stored, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.Equal(test, uint64(1), stored.FailoverCount)
			require.True(test, stored.LossyContextRestart)
			require.Equal(test, "account_session_capacity_full", stored.LastFailoverReason)
			previousResin := GetResinConfig()
			test.Cleanup(func() { SetResinConfig(previousResin) })
			type capture struct {
				headers http.Header
				body    []byte
			}
			sent := make(chan capture, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, outbound *http.Request) {
				sent <- capture{outbound.Header.Clone(), readUpstreamRequestBody(outbound)}
				_, _ = writer.Write([]byte(`{"id":"new-response"}`))
			}))
			test.Cleanup(server.Close)
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "capacity-test"})
			response, err := ExecuteRequest(request.Request.Context(), target, body, continuityTestThread, "", strings.TrimPrefix(request.GetHeader("Authorization"), "Bearer "), nil, request.Request.Header, false)
			require.NoError(test, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(test, err)
			require.NoError(test, response.Body.Close())
			outbound := <-sent
			assertSessionTools(test, outbound.body)
			require.Equal(test, "Bearer target-token", outbound.headers.Get("Authorization"))
			require.NotEqual(test, continuityTestThread, outbound.headers.Get("Session-Id"))
			require.NotContains(test, string(outbound.body), "old-encrypted")
			require.NotContains(test, string(outbound.body), "old-response")
			require.Contains(test, string(outbound.body), "keep this task")
			metadata := diagnosticMetadataObject(gjson.GetBytes(outbound.body, "client_metadata.x-codex-turn-metadata"))
			require.Equal(test, uint64(0), metadata.Get("window_number").Uint())
			require.Equal(test, outbound.headers.Get("Session-Id")+":0", metadata.Get("window_id").String())
			require.Contains(test, string(body), "old-encrypted", "do not mutate the inbound body")
		})
	}
}
