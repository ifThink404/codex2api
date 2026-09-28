package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestRelaxedRetryFailoverExcludedHealthyRoot(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		for _, failure := range []string{"upload_429", "http_500", "http_429", "stream_429", "transport", "first_token", "bare_exclusion", "invalid_request", "usage_policy", "blocked_replay"} {
			t.Run(fmt.Sprintf("%s/relaxed_%t", failure, relaxed), func(t *testing.T) {
				h, owner, target, key := failoverTestSetup(t, false)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				c, body := failoverTestRequest(t, h)
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				exclusions := newSessionRetryAccountExclusions(c, key, body)
				migratable := true
				switch failure {
				case "upload_429":
					exclusions.MarkRequestFailure(owner.ID(), ErrUpstream(429, "附件上传失败，请确认文件可由当前上游处理后重试。", nil), 1)
				case "http_500":
					exclusions.MarkHTTPFailure(owner.ID(), 500, []byte(`{"error":{"message":"temporary"}}`), 1, 1)
				case "http_429":
					exclusions.MarkHTTPFailure(owner.ID(), 429, []byte(`{"error":{"type":"rate_limit_exceeded"}}`), 1, 1)
				case "stream_429":
					exclusions.MarkStreamFailure(owner.ID(), streamOutcome{logStatusCode: 429, failureKind: "rate_limited"}, 1, 1)
				case "transport":
					exclusions.MarkTransportFailure(owner.ID(), 1)
				case "first_token":
					exclusions.MarkSoftFirstTokenTimeout(owner.ID())
				case "bare_exclusion":
					exclusions.MarkHard(owner.ID())
					migratable = false
				case "invalid_request":
					exclusions.MarkHTTPFailure(owner.ID(), 400, []byte(`{"error":{"type":"invalid_request_error"}}`), 1, 1)
					migratable = false
				case "usage_policy":
					exclusions.MarkHTTPFailure(owner.ID(), 403, []byte(`{"error":{"message":"403: This request was blocked by our usage policy."}}`), 1, 1)
					migratable = false
				case "blocked_replay":
					exclusions.MarkRequestFailure(owner.ID(), BlockTransportReplay(errors.New("payload already sent")), 1)
					migratable = false
				}
				require.Empty(t, sessionAccountFailoverReason(owner, auth.DispatchPolicyStandard), "request-local failure must not mark account exhausted")
				ctx, blocked := h.prepareSessionRetryFailover(c.Request.Context(), key, exclusions, auth.DispatchPolicyStandard)
				require.False(t, blocked)
				selected, _, handled := h.takeSessionAccountFailover(ctx, key, 0, exclusions.ForSelection(), nil, auth.DispatchPolicyStandard)
				wantSwitch := relaxed && migratable
				require.Equal(t, wantSwitch, handled)
				if wantSwitch {
					require.Same(t, target, selected)
					h.store.Release(selected)
					require.Equal(t, "request_excluded", usageRequestDiagnosticState(c).AccountFailover.TriggerReason)
					require.Equal(t, "relaxed_mode", usageRequestDiagnosticState(c).AccountFailover.EnabledBy)
				} else {
					require.Nil(t, selected)
				}
				record, _, err := h.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
				require.NoError(t, err)
				if wantSwitch {
					require.Equal(t, target.ID(), record.AccountID)
					require.EqualValues(t, 1, record.FailoverCount)
				} else {
					require.Equal(t, owner.ID(), record.AccountID)
					require.Zero(t, record.FailoverCount)
				}
			})
		}
	}
}

func TestRelaxedRetryFailoverRepeatedExclusions(t *testing.T) {
	h, owner, target, key := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	c, body := failoverTestRequest(t, h)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	exclusions := newSessionRetryAccountExclusions(c, key, body)
	h.store.AddAccount(&auth.Account{DBID: 1697, AccountID: "861373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "third-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}})
	for generation, failed := range []*auth.Account{owner, target} {
		exclusions.MarkHTTPFailure(failed.ID(), 429, []byte(`{"error":{"type":"rate_limit_exceeded"}}`), 2, 2)
		filter := auth.AccountFilter(nil)
		if generation == 0 {
			filter = func(a *auth.Account) bool { return a.ID() == target.ID() }
		}
		selected, _, _ := h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), key, 0, exclusions, filter, auth.DispatchPolicyStandard)
		require.NotNil(t, selected)
		require.EqualValues(t, 1696+generation, selected.ID())
		h.store.Release(selected)
		record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
		require.NoError(t, err)
		require.EqualValues(t, generation+1, record.FailoverCount)
	}
}

func TestRelaxedRetryFailureEvidenceClearedByTerminalFailure(t *testing.T) {
	r := newRetryAccountExclusions()
	r.MarkHTTPFailure(1, 429, nil, 1, 1)
	require.True(t, r.retryFailures[1])
	r.MarkHTTPFailure(1, 400, []byte(`{"error":{"type":"invalid_request_error"}}`), 1, 1)
	require.False(t, r.retryFailures[1])
}

func TestRelaxedRetryBPSImageUpload429SwitchesBeforeInference(t *testing.T) {
	// Explicit rollback mode retains the existing upload retry/failover contract.
	t.Setenv("CODEX_BPS_ATTACHMENT_429_FALLBACK", "off")
	h, owner, target, _ := failoverTestSetup(t, false)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	h.store.SetMaxRetries(0)
	h.store.SetMaxRateLimitRetries(10)
	h.store.SetRetryIntervalMS(1)
	h.store.SetTransportRetryPolicy("rotate")
	off := false
	owner.CodexNative, owner.CodexBPS = &off, true
	target.CodexNative, target.CodexBPS = &off, true
	var ownerUploads, targetUploads, inferences atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") == "Bearer owner-token" {
				ownerUploads.Add(1)
				w.WriteHeader(429)
				io.WriteString(w, `{"error":{"type":"rate_limit_exceeded"}}`)
				return
			}
			targetUploads.Add(1)
			io.WriteString(w, `{"openai_file_id":"file-relaxed-target"}`)
			return
		}
		inferences.Add(1)
		require.Equal(t, "Bearer target-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		stickyFailureSuccess(w)
	}))
	defer upstream.Close()
	previous := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "relaxed-upload-retry"})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	c, body := failoverTestRequest(t, h)
	// This test must miss the cache. Other fixtures use the same account IDs
	// and PNG; relying on their entries being evicted breaks with a larger LRU.
	imageData, err := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, err)
	imageData = append(imageData, []byte(NewUpstreamSessionUUID())...)
	body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"role":"user","content":[{"type":"input_text","text":"describe this picture"},{"type":"input_image","image_url":"data:image/png;base64,`+base64.StdEncoding.EncodeToString(imageData)+`"}]}]`))
	body, _ = sjson.SetBytes(body, "stream", true)
	c.Set(contextAPIKeyID, int64(101))
	c.Set(ingressRequestBodyContextKey, body)
	identity := h.resolveRequestSessionIdentityForContext(c, body)
	key := capacityAwareSessionAffinityKey(identity, 101)
	_, err = h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now(), UpstreamMode: "bps"})
	require.NoError(t, err)
	h.store.BindSessionAffinity(key, owner, "")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer test-user-key")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	h.Responses(c)
	require.Equal(t, 200, c.Writer.Status())
	require.EqualValues(t, 1, ownerUploads.Load())
	require.EqualValues(t, 1, targetUploads.Load())
	require.EqualValues(t, 1, inferences.Load())
	require.Equal(t, "switched", usageRequestDiagnosticState(c).AccountFailover.Result)
	record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.Equal(t, target.ID(), record.AccountID)
	require.Equal(t, "bps", record.UpstreamMode)
}
