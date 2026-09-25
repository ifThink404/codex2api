package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func freshRetryImage(t *testing.T) string {
	data, err := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, err)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(data, []byte(t.Name())...))
}

func TestBackgroundUploadRetryDetachesWithoutMovingParent(t *testing.T) {
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			h, parent, target, _ := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			h.store.SetMaxRetries(0)
			h.store.SetMaxRateLimitRetries(10)
			h.store.SetRetryIntervalMS(1)
			h.store.SetTransportRetryPolicy("rotate")
			off := false
			parent.CodexNative, parent.CodexBPS = &off, true
			target.CodexNative, target.CodexBPS = &off, true
			parent.SessionCapacityEnabled, parent.SessionCapacityMax = true, 2
			target.SessionCapacityEnabled, target.SessionCapacityMax = true, 2
			rootKey := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint("relaxed-parent"), 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(rootKey), database.SessionContinuityRecord{AccountID: parent.ID(), UpstreamMode: "bps", LastSeen: time.Now()})
			require.NoError(t, err)
			h.store.BindSessionAffinity(rootKey, parent, "")
			var parentUploads, targetUploads, inferences atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/attachments") {
					if r.Header.Get("Authorization") == "Bearer owner-token" {
						parentUploads.Add(1)
						w.WriteHeader(429)
						io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"Upload rate limit exceeded"}}`)
					} else {
						targetUploads.Add(1)
						io.WriteString(w, `{"openai_file_id":"file-retry-ok"}`)
					}
					return
				}
				inferences.Add(1)
				require.Equal(t, "Bearer target-token", r.Header.Get("Authorization"))
				if strings.HasSuffix(r.URL.Path, "/compact") {
					io.WriteString(w, `{"id":"resp_test","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					stickyFailureSuccess(w)
				}
			}))
			t.Cleanup(server.Close)
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "background-upload-retry"})
			imageURL := freshRetryImage(t)
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_text","text":"background task"},{"type":"input_image","image_url":%q}]}],"stream":true}`, imageURL))
			if path == "/v1/chat/completions" {
				body = []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":[{"type":"text","text":"background task"},{"type":"image_url","image_url":{"url":%q}}]}],"stream":true}`, imageURL))
			} else if path == "/v1/messages" {
				body = []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":[{"type":"text","text":"background task"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}],"max_tokens":16,"stream":true}`, strings.TrimPrefix(imageURL, "data:image/png;base64,")))
			}
			c, recorder := relaxedTestRequest(t, h, "thread_title", "resolved", body, path)
			_, verified := h.cachedNewAPIPolicyAuditState(c)
			remaining := int64(5000)
			verified.Meta.RootAccountWaitMillis = &remaining
			c.Set(newAPIPolicyMetaContextKey, verified)
			ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			c.Request = c.Request.WithContext(ensureTransportTrace(c.Request.Context()))
			map[string]func(*gin.Context){"/v1/responses": h.Responses, "/v1/responses/compact": h.ResponsesCompact, "/v1/chat/completions": h.ChatCompletions, "/v1/messages": h.Messages}[path](c)
			require.Equal(t, 200, recorder.Code, recorder.Body.String())
			require.EqualValues(t, 1, parentUploads.Load(), recorder.Body.String())
			require.EqualValues(t, 1, targetUploads.Load())
			require.EqualValues(t, 1, inferences.Load())
			h.db.FlushUsageLogs()
			logs, err := h.db.ListRecentUsageLogs(t.Context(), 10)
			require.NoError(t, err)
			require.Len(t, logs, 2)
			for _, entry := range logs {
				require.Equal(t, path, entry.Endpoint)
				if entry.StatusCode == http.StatusTooManyRequests {
					require.Equal(t, parent.ID(), entry.AccountID)
					require.Equal(t, "bps_attachment_upload", entry.UpstreamErrorKind)
					require.True(t, entry.IsRetryAttempt)
					require.Equal(t, 1, entry.AttemptIndex)
					require.Zero(t, entry.TotalTokens)
				}
			}
			fallback := usageRequestDiagnosticState(c).RelaxedFallback
			require.NotNil(t, fallback)
			require.Equal(t, "passive_retry_parent_excluded", fallback.Reason)
			stored, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(rootKey))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, parent.ID(), stored.AccountID)
			require.Zero(t, stored.FailoverCount)
			owner, found := h.store.AccountSessionAccountID(rootKey, time.Now())
			require.True(t, found)
			require.Equal(t, parent.ID(), owner)
			_, bound := h.store.SessionAffinityAccountID(fallback.key)
			require.False(t, bound)
		})
	}
}

func TestBPSUploadPoolExhaustionReturnsUploadError(t *testing.T) {
	h, owner, target, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	h.store.SetMaxRetries(0)
	h.store.SetMaxRateLimitRetries(10)
	h.store.SetRetryIntervalMS(1)
	h.store.SetTransportRetryPolicy("rotate")
	off := false
	owner.CodexNative, owner.CodexBPS = &off, true
	target.CodexNative, target.CodexBPS = &off, true
	var uploads, inferences atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if !strings.HasSuffix(r.URL.Path, "/attachments") {
			inferences.Add(1)
		}
		uploads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"Upload rate limit exceeded"}}`)
	}))
	t.Cleanup(server.Close)
	previous := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "upload-pool-failure"})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	c, body := failoverTestRequest(t, h)
	body, _ = sjson.SetRawBytes(body, "input", []byte(fmt.Sprintf(`[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]`, freshRetryImage(t))))
	body, _ = sjson.SetBytes(body, "stream", true)
	c.Set(contextAPIKeyID, int64(101))
	c.Set(ingressRequestBodyContextKey, body)
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	key := capacityAwareSessionAffinityKey(identity, 101)
	_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now(), UpstreamMode: "bps"})
	require.NoError(t, err)
	h.store.BindSessionAffinity(key, owner, "")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer test-key")
	c.Request = c.Request.WithContext(ensureTransportTrace(c.Request.Context()))
	h.Responses(c)
	require.Equal(t, 429, c.Writer.Status())
	require.EqualValues(t, 2, uploads.Load())
	require.Zero(t, inferences.Load())
	require.Equal(t, "no_safe_candidate", usageRequestDiagnosticState(c).AccountFailover.Result)
	diagnostic := bpsPreparationFailureForRequest(c)
	require.NotNil(t, diagnostic)
	require.Equal(t, "rate_limit_exceeded", diagnostic.detail.Code)
}

func TestBPSUploadFailureSurvivesSiblingCancellation(t *testing.T) {
	timing := &bpsTimingDiagnostic{}
	ctx := context.WithValue(t.Context(), bpsTimingContextKey{}, timing)
	headers := http.Header{"Authorization": {"Bearer sensitive-token"}}
	actual := bpsAttachmentFailure(ctx, headers, "private.png", "http", 429, http.Header{"X-Request-Id": {"upload-request"}}, []byte(`{"error":{"code":"rate_limit_exceeded","message":"Rate limit for private.png sensitive-token file-secret"}}`), nil)
	timing.uploaded(time.Millisecond, 10, 429, true)
	bpsAttachmentFailure(ctx, headers, "private.png", "canceled", 0, nil, nil, context.Canceled)
	timing.uploaded(time.Millisecond, 10, 0, true)
	encoded, err := json.Marshal(timing)
	require.NoError(t, err)
	require.EqualValues(t, 429, gjson.GetBytes(encoded, "upload_last_http_status").Int())
	require.EqualValues(t, 1, gjson.GetBytes(encoded, "upload_canceled").Int())
	require.Equal(t, "http", gjson.GetBytes(encoded, "upload_primary_error.stage").String())
	for _, secret := range []string{"private.png", "sensitive-token", "file-secret"} {
		require.NotContains(t, string(encoded), secret)
		require.NotContains(t, actual.Error(), secret)
	}
	status, payload, ok := continuousRetryHTTPErrorDetails(actual)
	require.True(t, ok)
	require.Equal(t, 429, status)
	require.Equal(t, "rate_limit_exceeded", gjson.GetBytes(payload, "error.code").String())
}

func TestBackgroundRetryFallbackRequiresAuthorizationAndRetryEvidence(t *testing.T) {
	for _, scenario := range []string{"retry", "strict", "bare_exclusion", "invalid_request", "usage_policy", "prompt_safety", "ticket", "canceled", "websocket"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, _, _ := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
				s.CodexForkAccountFallbackEnabled = scenario != "strict"
				return s
			})
			rootKey := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint("relaxed-parent"), 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(rootKey), database.SessionContinuityRecord{AccountID: owner.ID(), LastSeen: time.Now(), UpstreamMode: "bps"})
			require.NoError(t, err)
			h.store.BindSessionAffinity(rootKey, owner, "")
			body := []byte(`{"model":"gpt-5.6-sol","input":"background work"}`)
			c, _ := relaxedTestRequest(t, h, "memory_consolidation", "resolved", body)
			if scenario == "websocket" {
				c.Request.Method = http.MethodGet
				c.Request.Header.Set("Upgrade", "websocket")
				c.Request.Header.Set("Connection", "Upgrade")
			}
			identity := h.resolveRequestSessionIdentityForContext(c, body)
			require.True(t, identity.requiresRootAccount)
			key := capacityAwareSessionAffinityKey(identity, 101)
			oldKey := key
			beginDispatchSelection(c)
			selectionTraceForRequest(c).PinAccount(owner.ID())
			excluded := newSessionRetryAccountExclusions(c, key, body)
			switch scenario {
			case "bare_exclusion":
				excluded.MarkHard(owner.ID())
			case "invalid_request":
				excluded.MarkHTTPFailure(owner.ID(), 400, []byte(`{"error":{"type":"invalid_request_error"}}`), 1, 1)
			case "usage_policy":
				failure := bpsAttachmentFailure(c.Request.Context(), nil, "", "http", 403, nil, []byte(`{"error":{"message":"403: This request was blocked by our usage policy."}}`), nil)
				excluded.MarkRequestFailure(owner.ID(), failure, 1)
				require.False(t, excluded.retryFailures[owner.ID()])
			case "prompt_safety":
				failure := bpsAttachmentFailure(c.Request.Context(), nil, "", "http", 400, nil, []byte(`{"error":{"code":"invalid_prompt","message":"Your prompt was flagged as potentially violating our usage policy"}}`), nil)
				require.True(t, isHardStopUpstreamPolicyError(failure))
				excluded.MarkRequestFailure(owner.ID(), failure, 1)
			default:
				excluded.MarkRequestFailure(owner.ID(), ErrUpstream(429, "temporary upload failure", nil), 1)
			}
			if scenario == "ticket" {
				_, verified := h.cachedNewAPIPolicyAuditState(c)
				verified.Meta.WindowGrant = "explicit-ticket"
				c.Set(newAPIPolicyMetaContextKey, verified)
			}
			if scenario == "canceled" {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			changed, failure := h.prepareBackgroundRetryFallback(c, &identity, &key, body, excluded)
			require.Nil(t, failure)
			want := scenario == "retry" || scenario == "websocket"
			require.Equal(t, want, changed)
			if want {
				require.NotEqual(t, oldKey, key)
				require.False(t, identity.requiresRootAccount)
				require.Zero(t, selectionTraceForRequest(c).PinnedAccount())
			} else {
				require.Equal(t, oldKey, key)
				require.Equal(t, owner.ID(), selectionTraceForRequest(c).PinnedAccount())
			}
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(rootKey))
			require.NoError(t, err)
			require.Equal(t, owner.ID(), record.AccountID)
		})
	}
}

func TestBPSUploadCodeOnlyPolicyFailureRemainsTerminal(t *testing.T) {
	err := bpsAttachmentFailure(t.Context(), nil, "", "http", 403, nil, []byte(`{"error":{"code":"content_policy_violation","type":"invalid_request_error"}}`), nil)
	_, body, ok := continuousRetryHTTPErrorDetails(err)
	require.True(t, ok)
	require.Equal(t, "content_policy_violation", gjson.GetBytes(body, "error.code").String())
	excluded := newRetryAccountExclusions()
	excluded.MarkRequestFailure(1, err, 1)
	require.False(t, excluded.retryFailures[1])
}
