package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func relaxedSignedIdentityRequest(t *testing.T, h *Handler, body []byte, root string, canonical ...bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	meta := newAPIPolicyMeta{
		RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
		RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionID: root,
		RootSessionFingerprint: newAPIRootSessionFingerprint("test-platform", "42", root),
		ThreadSource:           "user", RequestKind: "turn",
	}
	if len(canonical) > 0 && !canonical[0] {
		meta.RootSessionID = ""
	}
	c, w := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, meta)
	c.Request.Header.Set("User-Agent", "Go-http-client/2.0")
	c.Request.Header.Set("Authorization", "Bearer test-user-key")
	if row, err := h.db.GetAPIKeyByValue(t.Context(), "test-user-key"); err == nil {
		c.Set(contextAPIKeyID, row.ID)
		c.Set(contextAPIKeyRow, row)
	}
	ctx, cancel := context.WithTimeout(ensureTransportTrace(c.Request.Context()), 8*time.Second)
	t.Cleanup(cancel)
	c.Request = c.Request.WithContext(ctx)
	c.Set(ingressRequestBodyContextKey, body)
	h.primeNewAPIPolicyContext(c, body)
	return c, w
}

func TestRelaxedSignedOnlyMissingOwnerReachesUpstream(t *testing.T) {
	for _, canonical := range []bool{true, false} {
		name := "signed_root_id"
		if !canonical {
			name = "signed_fingerprint_only"
		}
		t.Run(name, func(t *testing.T) { testRelaxedSignedOnlyMissingOwnerReachesUpstream(t, canonical) })
	}
}

func testRelaxedSignedOnlyMissingOwnerReachesUpstream(t *testing.T, canonical bool) {
	h, oldID, target, _, _, _ := missingOwnerSetup(t, "deleted")
	config := h.store.GetPromptFilterConfig()
	config.Advanced.NewAPI.Enabled = true
	h.store.SetPromptFilterConfig(config)
	h.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "test-platform", Secret: "integration-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit,
	}})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		stickyFailureSuccess(w)
	}))
	t.Cleanup(server.Close)
	previous := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "missing-identity-test"})
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
	c, w := relaxedSignedIdentityRequest(t, h, body, continuityTestThread, canonical)
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	require.True(t, identity.stableIdentity)
	require.NotEmpty(t, verifiedTransportUser(c.Request.Context()))
	key := capacityAwareSessionAffinityKey(identity, 101)
	_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{
		AccountID: oldID, UpstreamMode: "bps", LastSeen: time.Now(),
	})
	require.NoError(t, err)
	h.Responses(c)
	if w.Code != http.StatusOK {
		details, _ := json.Marshal(usageRequestDiagnosticState(c))
		t.Logf("dispatch diagnostics: %s", details)
	}
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.EqualValues(t, 1, calls.Load())
	d := usageRequestDiagnosticState(c).AccountFailover
	require.NotNil(t, d)
	require.Equal(t, "switched", d.Result)
	require.Equal(t, target.ID(), d.AccountID)
	require.NotContains(t, d.Selection.RejectionCounts, "outbound_identity_unavailable")
	record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, target.ID(), record.AccountID)
	require.EqualValues(t, 1, record.FailoverCount)
	// The next HTTP request restores the same mapping and durable owner.
	next, nextW := relaxedSignedIdentityRequest(t, h, body, continuityTestThread, canonical)
	h.Responses(next)
	require.Equal(t, http.StatusOK, nextW.Code, nextW.Body.String())
	require.EqualValues(t, 2, calls.Load())
}

func TestRelaxedSignedOnlyUpload429SwitchesAndResumes(t *testing.T) {
	// Explicit rollback mode retains the existing upload retry/failover contract.
	t.Setenv("CODEX_BPS_ATTACHMENT_429_FALLBACK", "off")
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_split", true: "cross_split"}[split], func(t *testing.T) {
			testRelaxedSignedOnlyUpload429SwitchesAndResumes(t, split)
		})
	}
}

func testRelaxedSignedOnlyUpload429SwitchesAndResumes(t *testing.T, split bool) {
	h, a, b, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexForkAccountFallbackEnabled = true
		s.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
		return s
	})
	config := h.store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = "off"
	h.store.SetPromptFilterConfig(config)
	h.store.SetMaxRetries(0)
	h.store.SetMaxRateLimitRetries(10)
	h.store.SetRetryIntervalMS(1)
	off := false
	for _, account := range []*auth.Account{a, b} {
		account.CodexNative, account.CodexBPS, account.CodexBPSProfile = &off, true, auth.BPSWord
		account.Models = []string{"gpt-6-astra"}
	}
	var first sync.Once
	var failingToken string
	var failures, inferences atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			first.Do(func() { failingToken = r.Header.Get("Authorization") })
			if r.Header.Get("Authorization") == failingToken {
				failures.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"429: Rate limit exceeded"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"openai_file_id":"file-signed-failover"}`)
			return
		}
		inferences.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		stickyFailureSuccess(w)
	}))
	t.Cleanup(server.Close)
	previous := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "signed-upload-test"})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	images, _ := concurrentBPSImageBody(t, int(time.Now().UnixNano()), 1)
	body := []byte(`{"model":"gpt-6-astra","stream":true}`)
	body, err := sjson.SetRawBytes(body, "input", []byte(gjson.GetBytes(images, "input").Raw))
	require.NoError(t, err)
	row := &database.APIKeyRow{ID: 101, AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}}}
	if split {
		row.ID, err = h.db.InsertAPIKeyWithOptions(t.Context(), database.APIKeyInput{Name: "cross-group-upload", Key: "test-user-key", AllowedGroupIDs: row.AllowedGroupIDs, Limits: row.Limits})
		require.NoError(t, err)
		h.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
			APIKeyID: row.ID, PlatformCode: "test-platform", Secret: "integration-secret", Enabled: true,
			PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit,
		}})
		a.GroupIDs, b.GroupIDs, a.SchedulerPriority = []int64{10}, []int64{20}, 100
		h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
		h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
	}
	c, w := relaxedSignedIdentityRequest(t, h, body, continuityTestThread)
	if split {
		c.Set(contextAPIKeyRow, row)
	}
	require.True(t, h.resolveRequestSessionIdentityForContext(c, body).stableIdentity)
	h.Responses(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.EqualValues(t, 1, failures.Load())
	require.EqualValues(t, 1, inferences.Load())
	d := usageRequestDiagnosticState(c).AccountFailover
	require.NotNil(t, d)
	require.Equal(t, "switched", d.Result)
	if split {
		require.Equal(t, b.ID(), d.AccountID)
		require.Equal(t, "relaxed_key_scope", usageRequestDiagnosticState(c).GroupRouting.Reason)
	}
	h.db.FlushUsageLogs()
	logs, err := h.db.ListRecentUsageLogs(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, logs, 2)
	statuses := make(map[int]int)
	accounts := make(map[int64]bool)
	for _, log := range logs {
		statuses[log.AttemptIndex] = log.StatusCode
		accounts[log.AccountID] = true
	}
	require.Equal(t, map[int]int{1: 429, 2: 200}, statuses)
	require.Len(t, accounts, 2)
	next, nextW := relaxedSignedIdentityRequest(t, h, body, continuityTestThread)
	if split {
		next.Set(contextAPIKeyRow, row)
	}
	h.Responses(next)
	require.Equal(t, http.StatusOK, nextW.Code, nextW.Body.String())
	require.EqualValues(t, 1, failures.Load(), "must not return to the failed owner")
	require.EqualValues(t, 2, inferences.Load())
}
