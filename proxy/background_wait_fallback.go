package proxy

import (
	"context"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

// Entry-time fallback cannot observe an owner/window disappearing during the
// wait. Reuse its authorization and detached-request machinery after the wait;
// do not take over the parent root or restart its wait budget.
func (h *Handler) waitForBackgroundRootWithFallback(c *gin.Context, identity *requestSessionIdentity, body []byte) *api.APIError {
	failure := h.waitForBackgroundRootAccount(c, *identity)
	if failure == nil || !CurrentRuntimeSettings().CodexForkAccountFallbackEnabled || c.Request.Context().Err() != nil {
		return failure
	}
	state := usageRequestDiagnosticState(c)
	reason := ""
	if failure.Code == api.ErrCodeRootAccountWaitTimeout {
		reason = "passive_wait_timeout"
	} else if failure.Code == api.ErrCodeBackgroundRootUnavailable {
		if d := state.BackgroundWindowWait; d != nil {
			switch d.Result {
			case "owner_changed", "account_unavailable", "unavailable":
				if d.Reason != "outbound_epoch_mismatch" {
					reason = "passive_wait_" + d.Result
				}
			case "validation_failed":
				if match := state.BackgroundAccountMatch; match != nil && match.Result == "root_owner_unavailable" {
					reason = "passive_wait_owner_unavailable"
				}
			}
		}
	}
	if reason == "" {
		return failure
	}
	root := h.resolveRequestRootSessionIdentityForContext(c, body)
	next := h.configureRelaxedAccountFallback(c, body, *identity, root, reason)
	if relaxedAccountFallbackFromContext(c.Request.Context()) == nil {
		return failure
	}
	// These snapshots refer to the old root. The temporary request gets its own
	// epoch at admission, after normal account/model/capacity checks succeed.
	ctx := context.WithValue(c.Request.Context(), backgroundAccountMatchContextKey{}, (*backgroundAccountMatch)(nil))
	ctx = context.WithValue(ctx, sessionOutboundEpochContextKey{}, (*sessionOutboundEpoch)(nil))
	c.Request = c.Request.WithContext(ctx)
	c.Set(apiRelaySessionExemptContextKey, false)
	*identity = next
	h.bindTurnStateSession(c, body, next)
	h.bindResponseIdentity(c, next)
	status, policy := h.cachedNewAPIPolicyAuditState(c)
	h.captureUsageRequestResolution(c, body, next, root, policy, status)
	state.RootAccountWait = "relaxed_fallback"
	if d := state.BackgroundWindowWait; d != nil {
		d.Result, d.Reason = "relaxed_fallback", reason
	}
	return prepareRelaxedAccountContext(c, body)
}
