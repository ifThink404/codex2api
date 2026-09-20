package proxy

import (
	"net/http"
	"time"

	"github.com/codex2api/auth"
)

// ApplyExplicitProbeError gives provider quota and permanent denial priority
// over optional API recovery. Both automatic and manual probes use this gate.
// The return value means the error was classified, not that the request worked.
func ApplyExplicitProbeError(store *auth.Store, account *auth.Account, status int, body []byte, resp *http.Response, model string) bool {
	if store == nil || account == nil {
		return false
	}
	if account.IsGrokAPI() {
		if IsGrokFreeQuotaExhaustedError(body) || status == http.StatusPaymentRequired || status == http.StatusForbidden || status == http.StatusTooManyRequests {
			applyGrokCooldown(store, account, status, body, resp, model)
			return true
		}
		return false
	}
	if IsDeactivatedWorkspaceError(body) {
		store.MarkDeactivatedWorkspace(account, upstreamAccountErrorMessage(status, body))
		return true
	}
	if IsAgentRuntimeDeletedError(body) {
		store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", upstreamAccountErrorMessage(status, body))
		return true
	}
	if account.IsAntigravityAPI() && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
		applyAntigravityCooldown(store, account, status, body, resp, model)
		return true
	}
	if IsUsageLimitReachedError(body) {
		Apply429Cooldown(store, account, body, resp, model)
		return true
	}
	return false
}
