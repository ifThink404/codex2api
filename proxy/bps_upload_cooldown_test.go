package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// An upload failure must survive request boundaries and trigger migration
// before another upload, even if no replacement was available on first failure.
func TestBPSUploadCooldownSkipsPersistedOwner(t *testing.T) {
	h, owner, target, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexForkAccountFallbackEnabled = true
		s.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
		return s
	})
	cfg := h.store.GetPromptFilterConfig()
	cfg.Advanced.Risk.SessionContinuityMode = "off"
	h.store.SetPromptFilterConfig(cfg)
	h.store.SetMaxRetries(0)
	h.store.SetMaxRateLimitRetries(10)
	h.store.SetRetryIntervalMS(1)
	h.store.SetTransportRetryPolicy("sticky")
	off := false
	owner.CodexNative, owner.CodexBPS = &off, true
	target.CodexNative, target.CodexBPS = &off, true
	atomic.StoreInt32(&target.Disabled, 1)
	var ownerUploads, targetUploads, inferences atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") == "Bearer owner-token" {
				ownerUploads.Add(1)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"429: Rate limit exceeded"}}`)
				return
			}
			targetUploads.Add(1)
			_, _ = io.WriteString(w, `{"openai_file_id":"file-owner-probe-ok"}`)
			return
		}
		inferences.Add(1)
		stickyFailureSuccess(w)
	}))
	t.Cleanup(upstream.Close)
	previous := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "owner-probe"})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	_, body := continuityTestRequest(86, "turn")
	images, _ := concurrentBPSImageBody(t, int(time.Now().UnixNano()), 29)
	var err error
	body, err = sjson.SetRawBytes(body, "input", []byte(gjson.GetBytes(images, "input").Raw))
	require.NoError(t, err)
	body, err = sjson.SetBytes(body, "stream", true)
	require.NoError(t, err)
	var key string
	router := gin.New()
	router.Use(h.ServiceErrorMiddleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Set(contextAPIKeyID, int64(101))
		c.Set(ingressRequestBodyContextKey, body)
		identity := h.resolveRequestSessionIdentityForContext(c, body)
		key = capacityAwareSessionAffinityKey(identity, 101)
		h.Responses(c)
	})
	request := func(want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-user-key")
		ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
		defer cancel()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req.WithContext(ctx))
		require.Equal(t, want, recorder.Code, recorder.Body.String())
		if want == http.StatusServiceUnavailable {
			require.Equal(t, "true", recorder.Header().Get("X-Should-Retry"), "temporary upload exhaustion must allow client retries")
			require.True(t, gjson.Get(recorder.Body.String(), "error.details.retryable").Bool())
		}
		h.db.FlushUsageLogs()
	}
	// The first request has only one usable account. Its upload returns 429.
	request(429)
	record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, owner.ID(), record.AccountID)
	owner.Mu().RLock()
	status, cooldown, failureStreak := owner.Status, owner.CooldownUtil, owner.FailureStreak
	owner.Mu().RUnlock()
	require.Equal(t, auth.StatusReady, status)
	require.True(t, cooldown.IsZero())
	require.Zero(t, failureStreak)
	require.True(t, h.bpsUploadCooldowns.get(bpsUploadCooldownKey(owner), time.Now()).After(time.Now()))
	// An empty eligible pool must not clear ownership or send another upload.
	before := ownerUploads.Load()
	request(503)
	require.Equal(t, before, ownerUploads.Load())
	afterEmpty, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.Equal(t, record.AccountID, afterEmpty.AccountID)
	require.Equal(t, record.FailoverCount, afterEmpty.FailoverCount)
	// A newly available healthy account must be selected without another
	// upload on the rate-limited persisted owner.
	atomic.StoreInt32(&target.Disabled, 0)
	before = ownerUploads.Load()
	request(200)
	require.Equal(t, before, ownerUploads.Load(), "must skip a previously upload-limited owner before sending")
	record, found, err = h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, target.ID(), record.AccountID)
	require.EqualValues(t, 29, targetUploads.Load())
	logs, err := h.db.ListRecentUsageLogs(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, logs, 2)
	ownerFailures, targetSuccess := 0, 0
	for _, entry := range logs {
		if entry.AccountID == owner.ID() {
			require.Equal(t, 429, entry.StatusCode)
			require.Equal(t, 1, entry.AttemptIndex)
			ownerFailures++
		} else {
			require.Equal(t, 200, entry.StatusCode)
			require.Equal(t, 1, entry.AttemptIndex)
			detail, readErr := h.db.GetUsageRequestDiagnostics(t.Context(), entry.ID)
			require.NoError(t, readErr)
			require.Equal(t, bpsUploadCooldownReason, gjson.GetBytes(detail.Diagnostics, "account_failover.reason").String())
			targetSuccess++
		}
	}
	require.Equal(t, 1, ownerFailures)
	require.Equal(t, 1, targetSuccess)
	t.Logf("request_2: owner %d skipped, target %d attempt=1 HTTP 200; all 29 historical images preserved", owner.ID(), target.ID())
	// After successful migration, the identical old window uses the new owner
	// and ready image handles; it does not hit the old owner again.
	before = ownerUploads.Load()
	request(200)
	require.Equal(t, before, ownerUploads.Load())
	require.EqualValues(t, 29, targetUploads.Load())
	require.EqualValues(t, 2, inferences.Load())
	t.Logf("request_3: HTTP 200 on target %d; upload calls=0 (cache reused); old owner not retried", target.ID())
}

func uploadCooldownTestError(t *testing.T, status int, retryAfter, body string) error {
	t.Helper()
	return bpsAttachmentFailure(t.Context(), nil, "", "http", status, http.Header{"Retry-After": {retryAfter}}, []byte(body), nil)
}

func TestBPSUploadCooldownTTLAndFailureScope(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter, body string
		status                 int
		want                   time.Duration
	}{
		{name: "default", status: 429, want: time.Minute},
		{name: "seconds", status: 429, retryAfter: "90", want: 90 * time.Second},
		{name: "bounded", status: 429, retryAfter: "999999", want: 5 * time.Minute},
		{name: "date", status: 429, retryAfter: time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat), want: 2 * time.Minute},
		{name: "invalid", status: 429, retryAfter: "injected\r\nheader", want: time.Minute},
		{name: "server", status: 500},
		{name: "client", status: 422},
		{name: "policy", status: 429, body: `{"error":{"code":"cyber_policy","message":"blocked"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			a := &auth.Account{DBID: 99281, AccountID: "upload-cooldown-ttl"}
			before := time.Now()
			h.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, tc.status, tc.retryAfter, tc.body))
			until := h.bpsUploadCooldowns.get(bpsUploadCooldownKey(a), before)
			if tc.want == 0 {
				require.True(t, until.IsZero())
				return
			}
			require.InDelta(t, tc.want.Seconds(), until.Sub(before).Seconds(), 2)
			require.True(t, h.bpsUploadCooldowns.get(bpsUploadCooldownKey(a), until.Add(time.Nanosecond)).IsZero(), "cooldown expires automatically")
		})
	}
}

func TestBPSUploadCooldownRequestScopeAndCache(t *testing.T) {
	h := &Handler{}
	a := &auth.Account{DBID: 99282, AccountID: "upload-cooldown-scope"}
	other := &auth.Account{DBID: 99283, AccountID: "other-cooldown-account"}
	url := freshRetryImage(t)
	image := fmt.Sprintf(`{"type":"input_image","image_url":%q}`, url)
	body := []byte(`{"input":[{"role":"user","content":[` + image + `]}]}`)
	h.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, 429, "60", ""))
	blocked := func(body []byte, mode string, account *auth.Account) bool {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		h.bindBPSUploadRequest(c, body, false)
		return bpsUploadCooldownForRequest(c.Request.Context(), account, mode)
	}
	require.True(t, blocked(body, "bps", a))
	require.False(t, blocked(body, "native", a))
	require.False(t, blocked(body, "bps", other))
	for _, text := range []string{
		`{"input":"text only"}`,
		`{"input":[{"type":"function_call","arguments":"{\"type\":\"input_image\"}"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/image.png"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-existing"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-existing"}]}]}`,
	} {
		require.False(t, blocked([]byte(text), "bps", a), text)
	}
	for _, text := range []string{
		`{"input":[{"type":"custom_tool_call_output","call_id":"c1","output":[` + image + `]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_data":"YQ==","filename":"note.txt"}]}]}`,
		fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}]}`, url),
		fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}`, strings.SplitN(url, ",", 2)[1]),
	} {
		require.True(t, blocked([]byte(text), "bps", a), text)
	}
	_, _, err := prepareBPSUserImageAttachments(t.Context(), a, body, nil, func(context.Context, []byte, string) (string, error) { return "file-cooldown-cached", nil })
	require.NoError(t, err)
	require.False(t, blocked(body, "bps", a), "ready handles need no upload; inference can proceed")
	// A text or cached success does not clear the cooldown for a new attachment.
	newBody, _ := concurrentBPSImageBody(t, 998882, 1)
	require.True(t, blocked(newBody, "bps", a))
}

func TestBPSUploadCooldownSharedAndMonotonic(t *testing.T) {
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	t.Cleanup(func() { _ = backend.Close() })
	h1, h2 := &Handler{cache: backend}, &Handler{cache: backend}
	a := &auth.Account{DBID: 99284, AccountID: "upload-cooldown-shared"}
	key := bpsUploadCooldownKey(a)
	h1.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, 429, "120", ""))
	first := h1.bpsUploadCooldowns.get(key, time.Now())
	h2.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, 429, "10", ""))
	require.True(t, h2.bpsUploadCooldowns.get(key, time.Now()).Equal(first), "a later short hint cannot shorten the active cooldown")
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, freshRetryImage(t)))
	h3 := &Handler{cache: backend}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	h3.bindBPSUploadRequest(c, body, false)
	require.True(t, bpsUploadCooldownForRequest(c.Request.Context(), a, "bps"), "a fresh handler observes the shared cooldown")
	parts := bpsUploadParts(body, nil, false, false)
	attachmentKey := bpsUploadPartKey(a, parts[0])
	raw, err := json.Marshal(bpsAttachmentRecord{Version: 1, ID: "file-shared-cooldown", Expires: time.Now().Add(time.Minute)})
	require.NoError(t, err)
	require.NoError(t, backend.SetRuntime(t.Context(), bpsAttachmentNamespace, attachmentKey, raw, time.Minute))
	ctx := WithBPSAttachmentCache(c.Request.Context(), backend)
	require.False(t, bpsUploadCooldownForRequest(ctx, a, "bps"), "shared attachment hits need no new upload")
}

func TestBPSUploadCooldownFailoverKeepsKeyScope(t *testing.T) {
	h, owner, target, key := failoverTestSetup(t, false)
	key += "-upload"
	h.store.BindSessionAffinity(key, owner, "")
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	off := false
	owner.CodexNative, owner.CodexBPS, owner.GroupIDs = &off, true, []int64{1}
	target.CodexNative, target.CodexBPS, target.GroupIDs = &off, true, []int64{2}
	h.store.SetAPIKeyAllowedGroups(101, []int64{1})
	_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now(), UpstreamMode: "bps"})
	require.NoError(t, err)
	h.rememberBPSUploadFailure(t.Context(), owner, uploadCooldownTestError(t, 429, "60", ""))
	c, body := failoverTestRequest(t, h)
	c.Set(contextAPIKeyID, int64(101))
	body, err = sjson.SetRawBytes(body, "input", []byte(fmt.Sprintf(`[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]`, freshRetryImage(t))))
	require.NoError(t, err)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 101, nil, nil, auth.DispatchPolicyStandard)
	require.True(t, handled)
	require.Nil(t, selected)
	record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, owner.ID(), record.AccountID)
	require.Positive(t, usageRequestDiagnosticState(c).AccountFailover.Selection.RejectionCounts["api_key_scope_mismatch"])
}

func TestBPSUploadCooldownFreshSelectionAndExpiry(t *testing.T) {
	h, a, b, _ := failoverTestSetup(t, false)
	off := false
	a.CodexNative, a.CodexBPS = &off, true
	b.CodexNative, b.CodexBPS = &off, true
	a.GroupIDs, b.GroupIDs = []int64{1}, []int64{2}
	h.store.SetAPIKeyAllowedGroups(101, []int64{1, 2})
	h.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, 429, "60", ""))
	c, body := continuityTestRequest(0, "turn")
	body, err := sjson.SetRawBytes(body, "input", []byte(fmt.Sprintf(`[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]`, freshRetryImage(t))))
	require.NoError(t, err)
	h.bindBPSUploadRequest(c, body, false)
	filter := codexRouteAccountFilter(c, nil)
	require.False(t, filter(a))
	require.True(t, filter(b))
	selected, _ := h.nextAccountForSessionWithFilter("fresh-upload-root", 101, nil, filter)
	require.Same(t, b, selected)
	h.store.Release(selected)
	h.bpsUploadCooldowns.mu.Lock()
	h.bpsUploadCooldowns.entries[bpsUploadCooldownKey(a)] = time.Now().Add(-time.Second)
	h.bpsUploadCooldowns.mu.Unlock()
	require.True(t, filter(a), "expired cooldown returns the account to attachment selection")
	trace := &auth.SelectionTrace{}
	trace.Reject(bpsUploadCooldownReason)
	require.Equal(t, "backoff_same_route", trace.Snapshot().Retry)
}

func TestBPSUploadCooldownPassiveFallbackPreservesParent(t *testing.T) {
	for _, textOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(textOnly), func(t *testing.T) {
			h, parent, _, _ := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			off := false
			parent.CodexNative, parent.CodexBPS = &off, true
			rootKey := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint("relaxed-parent"), 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(rootKey), database.SessionContinuityRecord{AccountID: parent.ID(), UpstreamMode: "bps", LastSeen: time.Now()})
			require.NoError(t, err)
			h.store.BindSessionAffinity(rootKey, parent, "")
			h.rememberBPSUploadFailure(t.Context(), parent, uploadCooldownTestError(t, 429, "60", ""))
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, freshRetryImage(t)))
			if textOnly {
				body = []byte(`{"model":"gpt-5.6-sol","input":"background text"}`)
			}
			c, _ := relaxedTestRequest(t, h, "thread_title", "resolved", body)
			identity := h.resolveRequestSessionIdentityForContext(c, body)
			fallback := relaxedAccountFallbackFromContext(c.Request.Context())
			if textOnly {
				require.Nil(t, fallback)
				require.True(t, identity.requiresRootAccount)
			} else {
				require.NotNil(t, fallback)
				require.Equal(t, "passive_parent_bps_upload_cooldown", fallback.Reason)
				require.False(t, identity.requiresRootAccount)
			}
			record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(rootKey))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, parent.ID(), record.AccountID)
			require.Zero(t, record.FailoverCount)
		})
	}
}

func TestBPSUploadCooldownCacheOutageAndConcurrency(t *testing.T) {
	backend := &failingBPSCache{sharedBPSMemory: sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}}
	t.Cleanup(func() { _ = backend.Close() })
	h := &Handler{cache: backend}
	a := &auth.Account{DBID: 99285, AccountID: "upload-cooldown-outage"}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 1; i <= 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.bpsUploadCooldowns.remember(bpsUploadCooldownKey(a), now.Add(time.Duration(i)*time.Second), now)
		}()
	}
	wg.Wait()
	require.True(t, h.bpsUploadCooldowns.get(bpsUploadCooldownKey(a), now).Equal(now.Add(100*time.Second)))
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, freshRetryImage(t)))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	h.bindBPSUploadRequest(c, body, false)
	require.True(t, bpsUploadCooldownForRequest(c.Request.Context(), a, "bps"), "cache outage must not erase a local observation")
	for i := int64(1); i <= 10; i++ {
		require.False(t, bpsUploadCooldownForRequest(c.Request.Context(), &auth.Account{DBID: i}, "bps"))
	}
	require.EqualValues(t, 1, backend.reads.Load(), "only one failing shared lookup per request")
}
