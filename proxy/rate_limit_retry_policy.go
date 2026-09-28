package proxy

import (
	"net/http"

	"github.com/codex2api/database"
)

func currentRateLimitRetryPolicy() string {
	if settings, ok := runtimeSettings.Load().(RuntimeSettings); ok {
		return database.NormalizeRateLimitRetryPolicy(settings.RateLimitRetryPolicy)
	}
	return database.RateLimitRetryRotate
}

func rateLimitRetryDisabled() bool {
	return currentRateLimitRetryPolicy() == database.RateLimitRetryOff
}

func rateLimitRequestError(err error) bool {
	status, _, ok := continuousRetryHTTPErrorDetails(err)
	return ok && status == http.StatusTooManyRequests
}

func retainRateLimitRequestAffinity(err error) bool {
	status, body, ok := continuousRetryHTTPErrorDetails(err)
	return ok && status == http.StatusTooManyRequests &&
		currentRateLimitRetryPolicy() != database.RateLimitRetryRotate &&
		!isPermanentAccountHTTPFailure(status, body)
}

// Dedicated media/native endpoints historically rotated every failure. Apply
// the new 429 choice there without changing their non-429 routing policy.
func (h *Handler) rateLimitFailureDisposition(status int, body []byte, retry bool, policy database.ContinuousRetryPolicy) sessionFailureDisposition {
	if status == http.StatusTooManyRequests {
		return h.httpSessionFailureDispositionForPolicy(status, body, retry, policy)
	}
	return sessionFailureDisposition{reportAccount: true}
}
