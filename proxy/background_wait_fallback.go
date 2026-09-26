package proxy

import (
	"context"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
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
	changed, fallbackError := h.detachBackgroundRequest(c, identity, body, reason)
	if !changed {
		return failure
	}
	return fallbackError
}

// Only an observed retryable failure may detach a live parent's child. Bare
// exclusions, policy refusals, explicit tickets and unverified labels retain
// their normal handling. The caller keeps its existing attempt/deadline budget.
func (h *Handler) prepareBackgroundRetryFallback(c *gin.Context, identity *requestSessionIdentity, key *string, body []byte, exclusions *retryAccountExclusions) (bool, *api.APIError) {
	if !CurrentRuntimeSettings().CodexForkAccountFallbackEnabled || !identity.requiresRootAccount ||
		relaxedAccountFallbackFromContext(c.Request.Context()) != nil || exclusions == nil || c.Request.Context().Err() != nil {
		return false, nil
	}
	rootKey, related := auth.RelatedSessionRootKey(*key)
	if !related {
		return false, nil
	}
	owner := int64(0)
	if match := backgroundAccountMatchFromContext(c.Request.Context()); match != nil {
		owner = match.accountID
	}
	if owner == 0 {
		owner, _ = h.store.LiveSessionAccountID(rootKey, time.Now())
	}
	if owner <= 0 || !exclusions.retryFailures[owner] || !exclusions.ForSelection()[owner] {
		return false, nil
	}
	changed, failure := h.detachBackgroundRequest(c, identity, body, "passive_retry_parent_excluded")
	if changed {
		*key = capacityAwareSessionAffinityKey(*identity, requestAPIKeyID(c))
		if exclusions.sessionFailover != nil {
			exclusions.sessionFailover.key = *key
		}
		c.Set(sessionContinuityContextKey, nil)
		selectionTraceForRequest(c).DetachSessionBinding()
	}
	return changed, failure
}

func (h *Handler) detachBackgroundRequest(c *gin.Context, identity *requestSessionIdentity, body []byte, reason string) (bool, *api.APIError) {
	root := h.resolveRequestRootSessionIdentityForContext(c, body)
	next := h.configureRelaxedAccountFallback(c, body, *identity, root, reason)
	if relaxedAccountFallbackFromContext(c.Request.Context()) == nil {
		return false, nil
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
	state := usageRequestDiagnosticState(c)
	state.RootAccountWait = "relaxed_fallback"
	if d := state.BackgroundWindowWait; d != nil {
		d.Result, d.Reason = "relaxed_fallback", reason
	}
	return true, prepareRelaxedAccountContext(c, body)
}
