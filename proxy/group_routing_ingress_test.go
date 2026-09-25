package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGroupRoutingActualIngressDefaultLowStillUsesSplit(t *testing.T) {
	for _, tc := range []struct {
		name, effort, reason, token string
		session                     bool
	}{
		{"default_low", "", "missing_reasoning_effort", "split-token", true},
		{"explicit_low", `,"reasoning":{"effort":"low"}`, "request_fingerprint", "test-token", true},
		{"missing_session", `,"reasoning":{"effort":"low"}`, "missing_session_id", "split-token", false},
		{"both_missing", "", "missing_session_and_reasoning_effort", "split-token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan string, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readUpstreamRequestBody(r)
				if got := extractReasoningEffort(body); got != "low" {
					t.Errorf("outbound effort = %q, want low", got)
				}
				seen <- r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, modelQuotaSSE)
			}))
			defer up.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, true)
			previousResin := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previousResin) })
			SetResinConfig(&ResinConfig{BaseURL: up.URL, PlatformName: "original-fields-test"})
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			configureRawRoutingTestGroups(h, row, false)
			h.store.AddAccount(&auth.Account{DBID: 2, GroupIDs: []int64{20}, AccessToken: "split-token", PlanType: "pro", Models: []string{"gpt-6-astra"}})
			headers := http.Header{}
			headers.Set("X-Codex2API-Affinity-Key", "original-client-affinity")
			if tc.session {
				session, err := uuid.NewV7()
				require.NoError(t, err)
				headers = nativeSessionHeaders(session.String(), session.String(), 0)
			}
			body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"` + tc.effort + `}`)
			c, w := rawRoutingTestContext(row, "/v1/responses", body, headers)
			h.Responses(c)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Len(t, seen, 1)
			require.Equal(t, "Bearer "+tc.token, <-seen)
			require.Equal(t, tc.reason, usageRequestDiagnosticState(c).GroupRouting.Reason)
		})
	}
}

func TestGroupRoutingUsesOriginalFieldsBeforeDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		session            bool
	}{
		{"absent_effort", `{}`, "missing_reasoning_effort", true},
		{"null_effort", `{"reasoning":{"effort":null}}`, "missing_reasoning_effort", true},
		{"empty_effort", `{"reasoning":{"effort":""}}`, "missing_reasoning_effort", true},
		{"blank_effort", `{"reasoning_effort":"  \t"}`, "missing_reasoning_effort", true},
		{"explicit_low", `{"reasoning":{"effort":"low"}}`, "request_fingerprint", true},
		{"chat_explicit_low", `{"reasoning_effort":"low"}`, "request_fingerprint", true},
		{"no_session", `{"reasoning":{"effort":"high"}}`, "missing_session_id", false},
		{"both_absent", `{}`, "missing_session_and_reasoning_effort", false},
		{"cache_key_is_not_session", `{"prompt_cache_key":"cache","reasoning_effort":"low"}`, "missing_session_id", false},
		{"task_is_not_session", `{"metadata":{"task_id":"task"},"reasoning_effort":"low"}`, "missing_session_id", false},
		{"flat_body_session", `{"client_metadata":{"session_id":"client"},"reasoning_effort":"low"}`, "request_fingerprint", false},
		{"body_thread", `{"client_metadata":{"thread_id":"client"},"reasoning_effort":"low"}`, "request_fingerprint", false},
		{"frame_body_session", `{"client_metadata":{"x-codex-turn-metadata":{"session_id":"client"}},"reasoning_effort":"low"}`, "request_fingerprint", false},
		{"metadata_effort_is_not_request_effort", `{"client_metadata":{"reasoning_effort":"low"}}`, "missing_reasoning_effort", true},
		{"history_effort_is_not_request_effort", `{"input":[{"type":"configuration_update","reasoning":{"effort":"low"}}]}`, "missing_reasoning_effort", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &database.APIKeyRow{AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}}}
			headers := http.Header{}
			headers.Set("X-Codex2API-Affinity-Key", "client-affinity")
			if tc.session {
				headers.Set("Session-Id", "client-session")
			}
			c, _ := rawRoutingTestContext(row, "/v1/responses", []byte(tc.body), headers)
			body, err := readRawRequestBody(c)
			require.NoError(t, err)
			// Exercise the real defaulting function, then simulate later middleware
			// reading the rewritten body and generated identity again.
			body = defaultCodexReasoning(body)
			setRawRequestBody(c, body)
			c.Request.Header.Set("Session-Id", "generated-upstream-session")
			_, err = readRawRequestBody(c)
			require.NoError(t, err)
			identity := resolveRequestSessionIdentity(c.Request.Header, body)
			require.True(t, identity.hasRequestFingerprint)
			filter := applyAffinityGroupRouting(c, identity, func(account *auth.Account) bool { return account.ID() != 3 })
			wantSplit := tc.reason != "request_fingerprint"
			require.Equal(t, wantSplit, filter(&auth.Account{DBID: 2, GroupIDs: []int64{20}}))
			require.Equal(t, !wantSplit, filter(&auth.Account{DBID: 1, GroupIDs: []int64{10}}))
			require.False(t, filter(&auth.Account{DBID: 3, GroupIDs: []int64{20}}), "must retain the inner account filter")
			require.Equal(t, tc.reason, usageRequestDiagnosticState(c).GroupRouting.Reason)
			// Splitting remains opt-in, including after an absent-input snapshot.
			row.Limits.NoAffinityGroupIDs = nil
			require.Nil(t, applyAffinityGroupRouting(c, identity, nil))
		})
	}
}

func TestGroupRoutingWebSocketFrameDoesNotInheritFields(t *testing.T) {
	row := &database.APIKeyRow{AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}}}
	headers := nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
	c, _ := rawRoutingTestContext(row, "/v1/responses", nil, headers)
	for _, tc := range []struct{ body, reason string }{
		{`{"model":"gpt-6-astra","reasoning":{"effort":"low"},"client_metadata":{"x-codex-turn-metadata":{"session_id":"first"}}}`, ""},
		{`{"model":"gpt-6-astra","client_metadata":{"x-codex-turn-metadata":{"session_id":"second"}}}`, "missing_reasoning_effort"},
		{`{"model":"gpt-6-astra","reasoning":{"effort":"low"},"client_metadata":{"x-codex-turn-metadata":{}}}`, "missing_session_id"},
		{`{"model":"gpt-6-astra","client_metadata":{"x-codex-turn-metadata":{}}}`, "missing_session_and_reasoning_effort"},
		{`{"model":"gpt-6-astra","reasoning_effort":"high","client_metadata":{"x-codex-turn-metadata":{"session_id":"new"}}}`, ""},
	} {
		body, _, apiErr := normalizeResponsesWebSocketClientPayload([]byte(tc.body))
		require.Nil(t, apiErr)
		setGroupRoutingIngress(c, body)
		captureGroupRoutingIngress(c, defaultCodexReasoning(body))
		require.Equal(t, tc.reason, missingGroupRoutingInputReason(c))
	}
}

func TestGroupRoutingReadsSessionFromOriginalTurnMetadataHeader(t *testing.T) {
	row := &database.APIKeyRow{}
	c, _ := rawRoutingTestContext(row, "/v1/responses", nil, http.Header{})
	c.Request.Header.Set(codexTurnMetadataHeader, `{"session_id":"client-session"}`)
	captureGroupRoutingIngress(c, []byte(`{"reasoning_effort":"low"}`))
	require.Empty(t, missingGroupRoutingInputReason(c))
}

func TestMissingGroupRoutingFieldsOverrideChatCohortButKeepOwnerErrors(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		h := newWindowAuthorizationHandler(t)
		c, body, identity := chatGroupTestRequest(t, h, "/v1/chat/completions")
		captureGroupRoutingIngress(c, body)
		primary := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
		split := &auth.Account{DBID: 2, GroupIDs: []int64{20}}
		h.store.AddAccount(split)
		if !unavailable {
			h.store.AddAccount(primary)
		}
		_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(sessionAffinityKey(identity.affinityID, 7)), database.SessionContinuityRecord{AccountID: 1})
		require.NoError(t, err)
		h.prepareChatGroupRouting(c, identity)
		filter := applyAffinityGroupRouting(c, identity, nil)
		require.False(t, filter(primary))
		require.Equal(t, !unavailable, filter(split))
		if unavailable {
			require.Equal(t, "group_routing_owner_unavailable", usageRequestDiagnosticState(c).GroupRouting.Reason)
		} else {
			require.Equal(t, "missing_reasoning_effort", usageRequestDiagnosticState(c).GroupRouting.Reason)
		}
	}
}
