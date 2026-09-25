package proxy

import (
	"context"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// The endpoint still owns its retry budget and the no-visible-output check.
// This state only prepares a migration when that endpoint selects another
// attempt after an observed quota failure, or after a retryable request-local
// exclusion in relaxed mode; it never initiates a retry itself.
type sessionQuotaRetry struct {
	request *gin.Context
	key     string
	body    []byte
}

func newSessionRetryAccountExclusions(request *gin.Context, key string, body []byte) *retryAccountExclusions {
	exclusions := newRetryAccountExclusions()
	exclusions.sessionQuota = &sessionQuotaRetry{request: request, key: key, body: body}
	return exclusions
}

func (r *retryAccountExclusions) noteQuotaFailure(accountID int64, status int, payload []byte) {
	if r == nil || accountID <= 0 || (status != http.StatusTooManyRequests && !IsUsageLimitReachedError(responseFailedErrorBody(payload))) {
		return
	}
	if r.quotaFailures == nil {
		r.quotaFailures = make(map[int64]bool)
	}
	r.quotaFailures[accountID] = true
}

func (r *retryAccountExclusions) noteRetryFailure(accountID int64) {
	if r == nil || accountID <= 0 {
		return
	}
	if r.retryFailures == nil {
		r.retryFailures = make(map[int64]bool)
	}
	r.retryFailures[accountID] = true
}

func (h *Handler) prepareSessionQuotaRetry(ctx context.Context, key string, exclusions *retryAccountExclusions, policy auth.DispatchPolicy) (context.Context, bool) {
	if exclusions == nil || exclusions.sessionQuota == nil || ctx.Err() != nil || sessionAccountFailoverEnabledBy("account_usage_exhausted") == "" {
		return ctx, false
	}
	retry := exclusions.sessionQuota
	request := retry.request
	if retry.key != key || request == nil || apiRelaySessionExempt(request) {
		return ctx, false
	}
	state := continuityRequest(request)
	if state == nil || state.Diagnostic == nil {
		return ctx, false
	}
	ownerID := state.Record.AccountID
	requestExcluded := CurrentRuntimeSettings().CodexForkAccountFallbackEnabled && exclusions.retryFailures[ownerID] && exclusions.ForSelection()[ownerID]
	if !exclusions.quotaFailures[ownerID] && !requestExcluded {
		return ctx, false
	}
	delete(exclusions.quotaFailures, ownerID)
	owner := h.store.FindByID(ownerID)
	reason := sessionAccountFailoverReason(owner, policy)
	if reason != "account_usage_exhausted" && reason != "account_spark_usage_exhausted" && !requestExcluded {
		return ctx, false
	}
	// A brand-new root had no owner when ingress diagnostics were created.
	// Its first attempt has since committed an authoritative owner in the DB.
	if state.Admitted && state.Diagnostic.OwnerAccount == 0 {
		state.Diagnostic.OwnerAccount = ownerID
	}
	// Reuse all normal switch checks (root ownership, complete context, exact
	// groups, grants and capacity) and the normal atomic epoch transition.
	excludedOwner := int64(0)
	if requestExcluded {
		excludedOwner = ownerID
	}
	pending, failure := h.prepareSessionAccountFailover(request, key, retry.body, policy, excludedOwner)
	if failure != nil {
		plan := &sessionAccountFailoverPlan{Request: request, Key: key, Body: retry.body, Failure: failure, Diagnostic: usageRequestDiagnosticState(request).AccountFailover}
		request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), sessionAccountFailoverContextKey{}, plan))
		return context.WithValue(ctx, sessionAccountFailoverContextKey{}, plan), true
	}
	if pending {
		plan, _ := request.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
		return context.WithValue(ctx, sessionAccountFailoverContextKey{}, plan), false
	}
	return ctx, false
}
