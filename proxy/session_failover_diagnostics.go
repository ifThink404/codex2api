package proxy

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

const sessionFailoverUnavailableMessage = "当前对话暂时无法继续处理请求，请稍后手动重试；若持续失败，请联系服务提供方。"
const sessionFailoverCapacityMessage = "当前可用账号容量暂时不足，请稍后重试当前对话。"

func failoverSelectionLabels(ctx *gin.Context, groups []int64, tags []string) ([]int64, []string, bool) {
	groups = slices.Clone(groups)
	slices.Sort(groups)
	groups = slices.Compact(groups)
	labels := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			labels = append(labels, tag)
		}
	}
	slices.Sort(labels)
	labels = slices.Compact(labels)
	truncated := len(groups) > 32 || len(labels) > 32
	groups, labels = groups[:min(len(groups), 32)], labels[:min(len(labels), 32)]
	for i, tag := range labels {
		labels[i] = serviceErrorSafeText(ctx, tag, 128)
		truncated = truncated || len(tag) > 128
	}
	return groups, labels, truncated
}

func sessionFailoverNoCandidate(ctx *gin.Context) bool {
	if ctx == nil || ctx.Request == nil {
		return false
	}
	plan, _ := ctx.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
	return plan != nil && plan.Diagnostic != nil && plan.Diagnostic.Result == "no_safe_candidate"
}

func sessionFailoverUnavailableAPIError(ctx *gin.Context) *api.APIError {
	message := sessionFailoverUnavailableMessage
	reason := "no_safe_candidate"
	if plan, _ := ctx.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan); plan != nil {
		if plan.Failure != nil {
			return plan.Failure
		}
		if plan.Diagnostic != nil {
			if plan.Diagnostic.Reason != "" {
				reason = plan.Diagnostic.Reason
			}
			if plan.Diagnostic.TriggerReason == bpsUploadCooldownReason {
				message = "附件上传暂时受限，当前没有可用的替代账号，请稍后重试。"
			}
		}
		if plan.Diagnostic != nil && plan.Diagnostic.Result == "no_safe_candidate" &&
			(plan.Diagnostic.TriggerReason == "account_session_capacity_full" || plan.Diagnostic.Reason == "account_session_capacity_full") {
			message = sessionFailoverCapacityMessage
		}
	}
	return api.NewAPIErrorWithDetails(api.ErrCodeServiceUnavailable, message, api.ErrorTypeServer,
		gin.H{"request_id": diagnosticRequestID(snapshotUpstreamTrace(ctx.Request.Context()).RequestID), "reason": reason, "retryable": true})
}

func sessionFailoverDispatchBlocked(ctx *gin.Context) bool {
	if ctx == nil || ctx.Request == nil {
		return false
	}
	plan, _ := ctx.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
	return plan != nil && (plan.Failure != nil || plan.Diagnostic != nil && (plan.Diagnostic.Result == "blocked" || plan.Diagnostic.Result == "no_safe_candidate"))
}

func sendSessionFailoverUnavailable(ctx *gin.Context, stream, chat bool) {
	protocol := continuousRetryProtocolResponses
	if chat {
		protocol = continuousRetryProtocolChat
	}
	if !claimContinuousRetryTerminal(ctx, protocol) {
		return
	}
	failure := sessionFailoverUnavailableAPIError(ctx)
	status := http.StatusBadRequest
	if plan, _ := ctx.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan); plan != nil && (plan.Failure != nil || failure.Code == api.ErrCodeServiceUnavailable) {
		status = api.HTTPStatusCode(failure.Code)
	}
	api.ObserveError(ctx, status, failure)
	if !ctx.Writer.Written() {
		retryHeader := "false"
		if details, ok := failure.Details.(gin.H); ok && details["retryable"] == true {
			retryHeader = "true"
		}
		ctx.Header("X-Should-Retry", retryHeader)
		ctx.JSON(status, api.ErrorResponse{Error: *failure})
		return
	}
	if stream {
		payload := gin.H{"error": failure}
		if !chat {
			payload = gin.H{"type": "response.failed", "response": gin.H{"status": "failed", "error": failure}}
		}
		body, _ := json.Marshal(payload)
		_, _ = ctx.Writer.WriteString("data: " + string(body) + "\n\n")
		ctx.Writer.Flush()
	}
}
