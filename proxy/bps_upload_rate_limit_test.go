package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSUploadRetryBudgetAndLogs(t *testing.T) {
	for _, tc := range []struct {
		name, policy                                         string
		general, rate, uploadStatus, wantStatus, wantUploads int
		allFail, file, websocket                             bool
	}{
		{name: "first_binding_rotate", policy: "rotate", rate: 10, uploadStatus: 429, wantStatus: 200, wantUploads: 2},
		{name: "first_binding_sticky", policy: "sticky", rate: 10, uploadStatus: 429, wantStatus: 200, wantUploads: 2},
		{name: "file_upload", policy: "rotate", rate: 10, uploadStatus: 429, wantStatus: 200, wantUploads: 2, file: true},
		{name: "websocket_first_binding", policy: "rotate", rate: 10, uploadStatus: 429, wantStatus: 200, wantUploads: 2, websocket: true},
		{name: "pool_exhausted", policy: "rotate", rate: 10, uploadStatus: 429, wantStatus: 429, wantUploads: 2, allFail: true},
		{name: "rate_budget_disabled", policy: "rotate", general: 10, uploadStatus: 429, wantStatus: 429, wantUploads: 1},
		{name: "server_error_general_budget", policy: "rotate", general: 1, uploadStatus: 500, wantStatus: 200, wantUploads: 2},
		{name: "server_error_no_general_budget", policy: "rotate", rate: 10, uploadStatus: 500, wantStatus: 500, wantUploads: 1},
		{name: "invalid_attachment", policy: "rotate", general: 10, rate: 10, uploadStatus: 422, wantStatus: 422, wantUploads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, a, b, _ := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
				s.CodexForkAccountFallbackEnabled = true
				s.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
				return s
			})
			cfg := h.store.GetPromptFilterConfig()
			cfg.Advanced.Risk.SessionContinuityMode = "off"
			h.store.SetPromptFilterConfig(cfg)
			h.store.SetMaxRetries(tc.general)
			h.store.SetMaxRateLimitRetries(tc.rate)
			h.store.SetRetryIntervalMS(1)
			h.store.SetTransportRetryPolicy(tc.policy)
			off := false
			a.CodexNative, a.CodexBPS = &off, true
			b.CodexNative, b.CodexBPS = &off, true
			var uploads, inferences atomic.Int32
			var first sync.Once
			var failingToken string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/attachments") {
					uploads.Add(1)
					first.Do(func() { failingToken = r.Header.Get("Authorization") })
					if tc.allFail || r.Header.Get("Authorization") == failingToken {
						w.Header().Set("X-Request-Id", "upload-retry-test")
						w.WriteHeader(tc.uploadStatus)
						_, _ = fmt.Fprintf(w, `{"error":{"type":"server_error","message":"attachment endpoint rejected %s file-secret private.txt"}}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
						return
					}
					_, _ = io.WriteString(w, `{"openai_file_id":"file-retry-ok"}`)
					return
				}
				inferences.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				stickyFailureSuccess(w)
			}))
			t.Cleanup(server.Close)
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "upload-rate-budget-test"})
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			_, body := continuityTestRequest(17, "turn")
			input := fmt.Sprintf(`[{"role":"user","content":[{"type":"input_text","text":"existing conversation history"},{"type":"input_image","image_url":%q}]}]`, freshRetryImage(t))
			if tc.file {
				input = `[{"role":"user","content":[{"type":"input_text","text":"existing conversation history"},{"type":"input_file","filename":"private.txt","file_data":"data:text/plain;base64,cHJpdmF0ZSBmaWxlIGNvbnRlbnQ="}]}]`
			}
			body, _ = sjson.SetRawBytes(body, "input", []byte(input))
			body, _ = sjson.SetBytes(body, "stream", true)
			var sessionKey string
			router := gin.New()
			router.Use(h.ServiceErrorMiddleware())
			finished := make(chan struct{})
			handle := func(c *gin.Context) {
				defer close(finished)
				c.Set(contextAPIKeyID, int64(101))
				c.Set(ingressRequestBodyContextKey, body)
				identity := h.resolveRequestSessionIdentityForContext(c, body)
				sessionKey = capacityAwareSessionAffinityKey(identity, 101)
				_, found, err := h.db.ReadSessionContinuity(c.Request.Context(), hashRiskIdentity(sessionKey))
				require.NoError(t, err)
				require.False(t, found, "old window is making its first request on this instance")
				if tc.websocket {
					h.ResponsesWebSocket(c)
				} else {
					h.Responses(c)
				}
			}
			router.POST("/v1/responses", handle)
			router.GET("/v1/responses", handle)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-user-key")
			req.Header.Set("X-Codex-Turn-State", "stale-state-from-another-instance")
			ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
			defer cancel()
			if tc.websocket {
				gateway := httptest.NewServer(router)
				t.Cleanup(gateway.Close)
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", req.Header)
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
				frame, err := sjson.SetBytes(body, "type", "response.create")
				require.NoError(t, err)
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(8*time.Second)))
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, frame))
				for {
					_, event, err := conn.ReadMessage()
					require.NoError(t, err)
					kind := gjson.GetBytes(event, "type").String()
					require.NotContains(t, []string{"error", "response.failed"}, kind, string(event))
					if kind == "response.completed" {
						break
					}
				}
				require.NoError(t, conn.Close())
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("WebSocket handler did not finish")
				}
			} else {
				router.ServeHTTP(recorder, req.WithContext(ctx))
				require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			}
			require.EqualValues(t, tc.wantUploads, uploads.Load())
			wantInference, failedAttempts := 0, tc.wantUploads
			if tc.wantStatus == http.StatusOK {
				wantInference, failedAttempts = 1, tc.wantUploads-1
			}
			require.EqualValues(t, wantInference, inferences.Load())
			h.db.FlushUsageLogs()
			logs, err := h.db.ListRecentUsageLogs(t.Context(), 10)
			require.NoError(t, err)
			require.Len(t, logs, tc.wantUploads, "each failed upload and final inference is logged once")
			failures := 0
			for _, entry := range logs {
				require.NotEmpty(t, entry.RequestID)
				require.Contains(t, []int64{a.ID(), b.ID()}, entry.AccountID)
				if entry.StatusCode == http.StatusOK {
					record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(sessionKey))
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, entry.AccountID, record.AccountID, "successful retry owns the persisted session")
					continue
				}
				failures++
				require.Equal(t, tc.uploadStatus, entry.StatusCode)
				require.Equal(t, "bps_attachment_upload", entry.UpstreamErrorKind)
				require.Contains(t, entry.ErrorMessage, "attachment endpoint rejected")
				require.Zero(t, entry.TotalTokens)
				require.Zero(t, entry.TotalCost)
				require.Zero(t, entry.UserBilled)
				require.Equal(t, tc.uploadStatus == 429 && tc.rate > 0 || tc.general > 0 && tc.uploadStatus == 500, entry.IsRetryAttempt)
				detail, err := h.db.GetUsageRequestDiagnostics(t.Context(), entry.ID)
				require.NoError(t, err)
				diagnostics := string(detail.Diagnostics)
				stage := "bps_image_preparation"
				if tc.file {
					stage = "bps_file_preparation"
				}
				require.Equal(t, stage, gjson.Get(diagnostics, "upstream.error_stage").String())
				require.Equal(t, "upload-retry-test", gjson.Get(diagnostics, "upstream.bps_compat.timing.upload_primary_error.request_id").String())
				require.Zero(t, gjson.Get(diagnostics, "upstream.bps_compat.timing.inference_attempts").Int())
				require.NotContains(t, diagnostics, "file-secret")
				require.NotContains(t, diagnostics, "owner-token")
				require.NotContains(t, diagnostics, "target-token")
				if tc.file {
					require.NotContains(t, diagnostics, "private.txt")
				}
			}
			require.Equal(t, failedAttempts, failures)
			events := serviceErrorTestPage(t, h).Items
			if tc.allFail {
				require.Len(t, events, 1, "record the final failed selection separately from upstream attempts")
				require.Equal(t, "codex_dispatch_bps_upload_retry_unavailable", events[0].Code)
			} else {
				require.Empty(t, events, "upstream attempts belong to usage logs, without duplicate service errors")
			}
		})
	}
}

func TestBPSUploadRetryCountersRemainIndependent(t *testing.T) {
	general, rate := 0, 0
	upload := fmt.Errorf("image preparation: %w", bpsAttachmentFailure(t.Context(), nil, "", "http", 429, nil, []byte(`{"error":{"message":"upload throttled"}}`), nil))
	for attempt := 0; attempt < 11; attempt++ {
		counter, limit := requestErrorRetryBudget(upload, &general, &rate, 1, 10)
		require.Equal(t, attempt < 10, shouldRetryRequestError(upload, counter, limit))
	}
	require.Equal(t, 10, rate)
	require.Zero(t, general)
	server := bpsAttachmentFailure(t.Context(), nil, "", "http", 500, nil, nil, nil)
	counter, limit := requestErrorRetryBudget(server, &general, &rate, 1, 10)
	require.True(t, shouldRetryRequestError(server, counter, limit))
	require.False(t, shouldRetryRequestError(server, counter, limit))
	require.Equal(t, 1, general)
	require.Equal(t, 10, rate)

	for _, unlimited := range []bool{false, true} {
		exclusions := newRetryAccountExclusions()
		limit := 10
		if unlimited {
			limit = -1
		}
		exclusions.MarkRequestFailure(1, upload, limit)
		require.True(t, exclusions.ForSelection()[1])
		require.True(t, exclusions.quotaFailures[1])
		require.True(t, exclusions.retryFailures[1])
		require.Equal(t, unlimited, exclusions.ResetTransient())
	}

	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	blocked := bpsAttachmentFailure(t.Context(), nil, "", "http", 429, nil, []byte(`{"error":{"code":"cyber_policy","message":"blocked"}}`), nil)
	counter, limit = requestErrorRetryBudget(blocked, &general, &rate, -1, -1)
	require.False(t, shouldRetryRequestError(blocked, counter, limit, policy))
	require.False(t, shouldRetryRequestError(BlockTransportReplay(upload), counter, limit, policy))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.False(t, isRetryableRequestErrorForContext(ctx, upload, policy))
	require.Equal(t, 10, rate)
}
