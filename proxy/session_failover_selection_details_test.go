package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestSessionFailoverCandidateDetailsAndTerminalTransports(t *testing.T) {
	for _, protocol := range []string{"http", "responses_sse", "chat_sse", "ws"} {
		t.Run(protocol, func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, true)
			h.store.SetSchedulerEngine("legacy")
			owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
			require.True(t, h.store.AdmitAccountSession(owner, "occupied", time.Now()))
			require.True(t, h.store.ApplyAccountGroups(owner.ID(), []int64{11}))
			require.True(t, h.store.ApplyAccountGroups(target.ID(), []int64{12}))
			require.True(t, h.store.ApplyAccountTags(owner.ID(), []string{"private-pool", "pro"}))
			require.True(t, h.store.ApplyAccountTags(target.ID(), []string{"pro"}))
			ctx, body := failoverTestRequest(t, h)
			require.Nil(t, h.configureSessionModelAffinity(ctx, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			selected, _, handled := h.takeSessionAccountFailover(ctx.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(t, handled)
			require.Nil(t, selected)
			require.True(t, sessionFailoverNoCandidate(ctx))
			diagnostic := continuityRequest(ctx).Diagnostic.AccountFailover
			require.Equal(t, []int64{11}, diagnostic.Selection.RequiredGroupIDs)
			require.Empty(t, diagnostic.Selection.RequiredTags)
			require.Equal(t, "exact_groups", diagnostic.Selection.MatchMode)
			require.Positive(t, diagnostic.Selection.RejectionCounts["account_groups_mismatch"])
			found := false
			for _, candidate := range diagnostic.Selection.Candidates {
				if candidate.AccountID == target.ID() && candidate.Reason == "account_groups_mismatch" {
					found = true
					require.Equal(t, []int64{12}, candidate.GroupIDs)
					require.Equal(t, []string{"pro"}, candidate.Tags)
				}
			}
			require.True(t, found)
			if protocol == "ws" {
				failure := h.dispatchUnavailableAPIError(ctx)
				require.Equal(t, api.ErrCodeServiceUnavailable, failure.Code)
				require.Equal(t, sessionFailoverCapacityMessage, failure.Message)
				require.Equal(t, 503, api.HTTPStatusCode(failure.Code))
				require.Equal(t, websocket.CloseTryAgainLater, responsesWSTerminalCloseCode(failure, websocket.CloseTryAgainLater))
				encoded, err := json.Marshal(failure)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "private-pool")
				return
			}
			recorder := httptest.NewRecorder()
			output, _ := gin.CreateTestContext(recorder)
			ctx.Writer = output.Writer
			finishAudit := h.beginServiceErrorAudit(ctx)
			serviceErrorAuditForRequest(ctx).authenticated = true
			stream := protocol != "http"
			if stream {
				ctx.Writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = ctx.Writer.WriteString(": ping\n\n")
			}
			h.sendDispatchUnavailable(ctx, stream, protocol == "chat_sse")
			require.Contains(t, recorder.Body.String(), "service_unavailable")
			require.Contains(t, recorder.Body.String(), sessionFailoverCapacityMessage)
			require.Contains(t, recorder.Body.String(), `"retryable":true`)
			require.NotContains(t, recorder.Body.String(), "private-pool")
			require.NotContains(t, recorder.Body.String(), "account_groups_mismatch")
			if !stream {
				require.Equal(t, http.StatusServiceUnavailable, ctx.Writer.Status())
				require.Equal(t, "true", ctx.Writer.Header().Get("X-Should-Retry"))
			} else {
				require.Equal(t, http.StatusOK, ctx.Writer.Status())
				if protocol == "responses_sse" {
					require.Contains(t, recorder.Body.String(), "response.failed")
				}
			}
			finishAudit()
			page := serviceErrorTestPage(t, h)
			require.Len(t, page.Items, 1)
			require.Equal(t, "service_unavailable", page.Items[0].Code)
			require.Equal(t, sessionFailoverCapacityMessage, page.Items[0].Message)
			require.Equal(t, 503, page.Items[0].StatusCode)
			require.Equal(t, "dispatch", page.Items[0].Stage)
			require.NotNil(t, page.Items[0].AccountFailover.Selection)
			require.Empty(t, page.Items[0].AccountFailover.Selection.RequiredTags)
			require.Equal(t, "exact_groups", page.Items[0].AccountFailover.Selection.MatchMode)
		})
	}
}
