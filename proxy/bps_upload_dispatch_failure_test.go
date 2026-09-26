package proxy

import (
	"bytes"
	"context"
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

// A migrated old window can log just one upstream attempt even when its retry
// budget is available. Preserve the final selection outcome separately so an
// unavailable or ineligible replacement is distinguishable from another 429.
func TestBPSUploadRetryFinalSelectionDiagnostics(t *testing.T) {
	for _, reason := range []string{"healthy", "account_disabled", "different_groups", "outside_key_groups", "all_uploads_limited"} {
		t.Run(reason, func(t *testing.T) {
			h, owner, target, _ := failoverTestSetup(t, false)
			keyID, err := h.db.InsertAPIKey(t.Context(), "bps-dispatch-test", "test-user-key")
			require.NoError(t, err)
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
			h.store.SetRelaxedAccountGroups(true)
			off := false
			owner.CodexNative, owner.CodexBPS = &off, true
			target.CodexNative, target.CodexBPS = &off, true
			if reason == "account_disabled" {
				atomic.StoreInt32(&target.Disabled, 1)
			} else if reason == "different_groups" || reason == "outside_key_groups" {
				owner.GroupIDs = []int64{1}
				target.GroupIDs = []int64{999}
			}
			if reason == "outside_key_groups" {
				h.store.SetAPIKeyAllowedGroups(keyID, []int64{1})
				h.store.SetAPIKeyNoAffinityGroups(keyID, []int64{2})
			}

			var targetUploads, inferences atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if strings.HasSuffix(r.URL.Path, "/attachments") {
					w.Header().Set("Content-Type", "application/json")
					if r.Header.Get("Authorization") == "Bearer target-token" {
						targetUploads.Add(1)
						if reason != "all_uploads_limited" {
							_, _ = io.WriteString(w, `{"openai_file_id":"file-dispatch-ok"}`)
							return
						}
					}
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"429: Rate limit exceeded"}}`)
					return
				}
				inferences.Add(1)
				stickyFailureSuccess(w)
			}))
			t.Cleanup(upstream.Close)
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "bps-dispatch-test"})
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")

			_, body := continuityTestRequest(86, "turn")
			images, _ := concurrentBPSImageBody(t, int(time.Now().UnixNano()), 29)
			body, _ = sjson.SetRawBytes(body, "input", []byte(gjson.GetBytes(images, "input").Raw))
			body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"compaction","encrypted_content":"opaque-history"}`))
			body, _ = sjson.SetBytes(body, "stream", true)
			router := gin.New()
			router.Use(h.ServiceErrorMiddleware())
			router.POST("/v1/responses", func(c *gin.Context) {
				c.Set(contextAPIKeyID, keyID)
				if reason == "outside_key_groups" {
					c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: keyID, AllowedGroupIDs: []int64{1}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{2}}})
				}
				c.Set(ingressRequestBodyContextKey, body)
				identity := h.resolveRequestSessionIdentityForContext(c, body)
				key := capacityAwareSessionAffinityKey(identity, keyID)
				_, err := h.db.CommitSessionContinuity(c.Request.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{
					AccountID: owner.ID(), ThreadID: continuityTestThread, Number: 86, NumberKnown: true,
					LastSeen: time.Now(), UpstreamMode: "bps", FailoverCount: 1, OutboundWindowReset: true,
					LossyContextRestart: true, LastFailoverReason: "continuity_unbound_nonzero",
				})
				require.NoError(t, err)
				h.store.BindSessionAffinity(key, owner, "")
				h.Responses(c)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-user-key")
			req.Header.Set("X-NewAPI-Request-ID", "bps-dispatch-correlation")
			ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
			defer cancel()
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req.WithContext(ctx))
			h.db.FlushUsageLogs()
			logs, err := h.db.ListRecentUsageLogs(t.Context(), 10)
			require.NoError(t, err)
			page := serviceErrorTestPage(t, h)
			if reason == "healthy" || reason == "different_groups" || reason == "outside_key_groups" {
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.Len(t, logs, 2)
				require.Empty(t, page.Items)
				require.EqualValues(t, 29, targetUploads.Load())
				require.EqualValues(t, 1, inferences.Load())
				for _, entry := range logs {
					if entry.StatusCode == http.StatusOK {
						detail, err := h.db.GetUsageRequestDiagnostics(t.Context(), entry.ID)
						require.NoError(t, err)
						require.Equal(t, "relaxed_no_groups", gjson.GetBytes(detail.Diagnostics, "account_failover.selection.match_mode").String())
					}
				}
				return
			}
			require.Equal(t, http.StatusTooManyRequests, recorder.Code, recorder.Body.String())
			require.Equal(t, "upstream_error", gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
			require.Zero(t, inferences.Load())
			if reason == "all_uploads_limited" {
				require.Len(t, logs, 2, "both accounts were actually attempted")
				require.Positive(t, targetUploads.Load())
			} else {
				require.Len(t, logs, 1, "selection is not a second upstream attempt")
				require.Zero(t, targetUploads.Load())
			}
			for _, log := range logs {
				require.True(t, log.IsRetryAttempt)
				require.Zero(t, log.TotalTokens)
				require.Zero(t, log.TotalCost)
			}
			require.Len(t, page.Items, 1, "the final failed selection must remain observable")
			event := page.Items[0]
			require.Equal(t, "codex_dispatch_bps_upload_retry_unavailable", event.Code)
			require.Equal(t, "dispatch", event.Stage)
			require.Equal(t, http.StatusTooManyRequests, event.StatusCode)
			require.Equal(t, diagnosticRequestID("bps-dispatch-correlation"), event.NewAPIRequestID)
			require.NotNil(t, event.AccountFailover)
			require.Equal(t, "no_safe_candidate", event.AccountFailover.Result)
			require.NotNil(t, event.AccountFailover.Selection)
			if reason != "all_uploads_limited" {
				require.Positive(t, event.AccountFailover.Selection.RejectionCounts[reason])
			}
			require.Equal(t, "bps_image_preparation", gjson.GetBytes(event.UpstreamInfo, "error_stage").String())
			require.NotContains(t, string(event.UpstreamInfo), "owner-token")
			require.NotContains(t, string(event.UpstreamInfo), "target-token")
		})
	}
}
