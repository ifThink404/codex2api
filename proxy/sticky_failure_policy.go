package proxy

import (
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// sessionFailureDisposition separates retry routing from affinity lifetime.
// The old implementation coupled both decisions by unbinding before it knew
// whether a retry would happen, which made a client-side retry migrate the
// whole conversation even when the configured retry budget was zero.
type sessionFailureDisposition struct {
	retrySameAccount bool
	retainAffinity   bool
	pinAffinity      bool
	reportAccount    bool
	permanentAccount bool
}

func isPermanentAccountHTTPFailure(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusUpgradeRequired:
		return true
	case http.StatusBadRequest:
		return isCodexModelUnsupportedError(body)
	}
	return IsDeactivatedWorkspaceError(body) || IsUsageLimitReachedError(body)
}

func isRequestScopedHTTPFailure(statusCode int, body []byte) bool {
	if statusCode < 400 || statusCode >= 500 || statusCode == http.StatusTooManyRequests {
		return false
	}
	return !isPermanentAccountHTTPFailure(statusCode, body)
}

func (h *Handler) httpSessionFailureDisposition(statusCode int, body []byte, shouldRetry bool) sessionFailureDisposition {
	if isUpstreamPromptSafetyRefusal(body) {
		return sessionFailureDisposition{retainAffinity: true}
	}
	permanent := isPermanentAccountHTTPFailure(statusCode, body)
	requestScoped := isRequestScopedHTTPFailure(statusCode, body)
	sticky := h.stickyTransportRetryEnabled()
	if statusCode == http.StatusTooManyRequests {
		sticky = currentRateLimitRetryPolicy() != database.RateLimitRetryRotate
		shouldRetry = shouldRetry && !rateLimitRetryDisabled()
	}
	retrySame := sticky && shouldRetry && !permanent
	retain := requestScoped || (sticky && !permanent)
	return sessionFailureDisposition{
		retrySameAccount: retrySame,
		retainAffinity:   retain,
		pinAffinity:      sticky && !permanent && !requestScoped,
		reportAccount:    !requestScoped && !retrySame,
		permanentAccount: permanent,
	}
}

func (h *Handler) streamSessionFailureDisposition(outcome streamOutcome, payload []byte, shouldRetry bool) sessionFailureDisposition {
	body := payload
	if len(body) == 0 {
		body = outcome.failurePayload
	}
	if len(body) == 0 && outcome.failureMessage != "" {
		body = []byte(outcome.failureMessage)
	}
	status := outcome.logStatusCode
	if streamOutcomeUsesRateLimitBudget(outcome) {
		status = http.StatusTooManyRequests
	}
	disposition := h.httpSessionFailureDisposition(status, body, shouldRetry)
	switch outcome.failureKind {
	case "usage_limit", "spark_usage_limit", "rate_limited_5h", "rate_limited_7d":
		return sessionFailureDisposition{reportAccount: true, permanentAccount: true}
	}
	return disposition
}

func (h *Handler) httpSessionFailureDispositionForUpstream(account *auth.Account, resp *http.Response, body []byte, shouldRetry bool, policy database.ContinuousRetryPolicy) sessionFailureDisposition {
	disposition := h.httpSessionFailureDispositionForPolicy(resp.StatusCode, body, shouldRetry, policy)
	if account != nil && !account.IsRelayStyle() && resp.StatusCode == http.StatusTooManyRequests {
		// An actual HTTP/dial 429 may identify the exhausted window only in
		// response headers. This is not the successful stream's usage snapshot.
		window, _, _ := classifyCodex429Window(resp, time.Now())
		if window != codexRateLimitWindowUnknown {
			return sessionFailureDisposition{reportAccount: true, permanentAccount: true}
		}
	}
	return disposition
}

// A temporary 429 must not cool the owner before a permitted same-account
// retry selects it again. Quota exhaustion and Spark's separately classified
// window limits are still applied immediately.
func (h *Handler) deferStickyStream429Cooldown(account *auth.Account, outcome streamOutcome, payload []byte, model string, retryPossible bool, policy database.ContinuousRetryPolicy) bool {
	return retryPossible && account != nil && !account.IsRelayStyle() && !isProOnlyModel(model) &&
		outcome.logStatusCode == http.StatusTooManyRequests &&
		h.streamSessionFailureDispositionForPolicy(outcome, payload, true, policy).retrySameAccount
}

func (h *Handler) pendingStickyStream429Retry(account *auth.Account, outcome streamOutcome, payload []byte, model string, generalRetries, rateLimitRetries, maxRetries, maxRateLimitRetries int, wrote bool, contextErr, writeErr error, policy database.ContinuousRetryPolicy) bool {
	if !h.deferStickyStream429Cooldown(account, outcome, payload, model, true, policy) {
		return false
	}
	// Copies of the counters: only the actual retry branch consumes the budget.
	return shouldTransparentRetryStreamWithBudgets(outcome, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries, wrote, contextErr, writeErr, policy)
}

func (h *Handler) transportSessionFailureDisposition(shouldRetry bool, allowSticky bool) sessionFailureDisposition {
	sticky := allowSticky && h.stickyTransportRetryEnabled()
	return sessionFailureDisposition{
		retrySameAccount: sticky && shouldRetry,
		retainAffinity:   sticky,
		pinAffinity:      sticky,
		reportAccount:    !sticky || !shouldRetry,
	}
}

func (h *Handler) applySessionFailureAffinity(affinityKey string, account *auth.Account, disposition sessionFailureDisposition) {
	if h == nil || h.store == nil || account == nil {
		return
	}
	if disposition.retainAffinity {
		if disposition.pinAffinity {
			h.store.PinSessionAffinityAfterTransientFailure(affinityKey, account.ID())
		}
		return
	}
	h.store.UnbindSessionAffinity(affinityKey, account.ID())
}
