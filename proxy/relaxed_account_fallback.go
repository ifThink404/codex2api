package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type relaxedAccountFallbackKey struct{}

// A temporary child request owns a private scheduling key, never its missing
// parent's key. It cannot establish a persistent root or borrow a root lease.
type relaxedAccountFallback struct {
	Reason          string                          `json:"reason"`
	ParentAccountID int64                           `json:"parent_account_id,omitempty"`
	AccountID       int64                           `json:"account_id,omitempty"`
	Temporary       bool                            `json:"temporary"`
	Cleanup         *database.SessionContextCleanup `json:"context_cleanup,omitempty"`
	key             string
	preserveInput   bool
	references      []string
	accounts        map[int64]bool
	generation      uint64
}

func relaxedAccountFallbackFromContext(ctx context.Context) *relaxedAccountFallback {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(relaxedAccountFallbackKey{}).(*relaxedAccountFallback)
	return state
}

func (h *Handler) configureRelaxedAccountFallback(c *gin.Context, body []byte, identity requestSessionIdentity, root requestRootSessionIdentity, waitReason ...string) requestSessionIdentity {
	// Use the existing persisted switch so upgrades retain the administrator's
	// choice. Conflicting identity and explicit window tickets remain strict.
	if !CurrentRuntimeSettings().CodexForkAccountFallbackEnabled || !identity.requiresRootAccount || root.conflict || h.store == nil || windowGrantForRequest(c) != nil || c.Request.Context().Err() != nil {
		return identity
	}
	path := c.Request.URL.Path
	if !strings.HasSuffix(path, "/responses") && !strings.HasSuffix(path, "/responses/compact") && !strings.HasSuffix(path, "/chat/completions") && !strings.HasSuffix(path, "/messages") {
		return identity
	}
	status, policy := h.cachedNewAPIPolicyAuditState(c)
	verified := (status == "verified" || status == "signed_response") && policy.MetaVerified
	if !verified && !root.nativeRoot {
		// A bare, unverified non-user label is not authorization to create an
		// independent background request outside user-window accounting.
		return identity
	}
	if policy.MetaVerified && strings.TrimSpace(policy.Meta.WindowGrant) != "" {
		// Ticket validation runs after identity resolution on HTTP ingress.
		// Do not temporarily detach before that authorization is checked.
		return identity
	}
	parentID, mode, reason := int64(0), "", "passive_parent_missing"
	afterWait := len(waitReason) == 1 && waitReason[0] != ""
	if identity.stableIdentity && identity.affinityID != "" {
		key := sessionAffinityKey(identity.affinityID, requestAPIKeyID(c))
		entry, found, err := h.readSessionContinuity(c.Request.Context(), hashRiskIdentity(key))
		if err != nil {
			return identity // A failed owner lookup is not a missing parent.
		}
		if found {
			parentID, mode = entry.Record.AccountID, entry.Record.UpstreamMode
		} else {
			parentID, _ = h.store.LiveSessionAccountID(key, time.Now())
		}
		if parent := h.store.FindByID(parentID); parent != nil && !afterWait {
			h.bindBPSUploadRequest(c, body, strings.HasSuffix(path, "/compact"))
			model := gjson.GetBytes(body, "model").String()
			failure := h.sessionOwnerFailure(c, parent, key, dispatchPolicyForModel(model), codexRouteRequest{Model: model, Auxiliary: true}, mode)
			if failure == "" {
				return identity
			}
			reason = "passive_parent_" + strings.TrimPrefix(failure, "account_")
			if failure == "account_session_capacity_full" {
				reason = "passive_parent_capacity_full"
			}
		}
	}
	if afterWait {
		reason = waitReason[0]
	}
	state := &relaxedAccountFallback{Reason: reason, ParentAccountID: parentID, Temporary: true, preserveInput: CurrentRuntimeSettings().CodexSessionFailoverPreserveInput, accounts: make(map[int64]bool)}
	state.references = sortedCodexParentReferences(sessionFailoverRequestHeaders(c), body)
	identity.affinityID = "temporary-passive:" + NewUpstreamSessionUUID()
	identity.stableIdentity, identity.relatedToRoot, identity.ownsRootBinding = false, false, false
	identity.requiresRootAccount, identity.protectedRelatedLease, identity.bypassWindowAccounting = false, false, false
	identity.unlinkedFallbackOnly, identity.forkSourceAffinityID, identity.apiRelayCapacityKey = false, "", ""
	state.key = capacityAwareSessionAffinityKey(identity, requestAPIKeyID(c))
	ctx := context.WithValue(c.Request.Context(), relaxedAccountFallbackKey{}, state)
	if mode == "bps" {
		ctx = context.WithValue(ctx, codexRouteFloorKey{}, "bps")
	}
	c.Request = c.Request.WithContext(ctx)
	c.Set(relatedSessionObservationContextKey, nil)
	usageRequestDiagnosticState(c).RelaxedFallback = state
	return identity
}

func (h *Handler) cleanupRelaxedAccountFallback(c *gin.Context) {
	state := relaxedAccountFallbackFromContext(c.Request.Context())
	if state == nil {
		// Keepalive/deadline defers restore their ingress context before this
		// cleanup. A fallback created during a retry only exists in the newer
		// context, but its request-owned diagnostic still holds the lease keys.
		state = usageRequestDiagnosticState(c).RelaxedFallback
	}
	if state == nil {
		return
	}
	for id := range state.accounts {
		h.store.UnbindSessionAffinity(state.key, id)
	}
}

func prepareRelaxedAccountContext(c *gin.Context, body []byte) *api.APIError {
	state := relaxedAccountFallbackFromContext(c.Request.Context())
	if state == nil {
		return nil
	}
	var failure *api.APIError
	body, failure = sessionReplayBody(c, body)
	if failure != nil {
		return failure
	}
	_, _, report, err := cleanSessionRestartContext(sessionFailoverRequestHeaders(c), body, nil, state.preserveInput)
	state.Cleanup = report
	if err != nil {
		if failure := sessionToolPreservationAPIError(err, report); failure != nil {
			return failure
		}
		return api.NewAPIError("codex_session_restart_context_required", err.Error(), api.ErrorTypeInvalidRequest)
	}
	return nil
}

func (h *Handler) commitRelaxedAccountFallback(c *gin.Context, account *auth.Account, state *relaxedAccountFallback) *api.APIError {
	if account == nil || account.IsRelayStyle() {
		return sessionContinuityError("ownership_unavailable")
	}
	info := codexRouteRequestInfo(c.Request.Context())
	floor, _ := c.Request.Context().Value(codexRouteFloorKey{}).(string)
	mode := selectCodexRoute(account, info.Model, floor, info.Auxiliary)
	if mode == "" {
		return sessionModelUnavailableError(c)
	}
	if state.AccountID != account.ID() || state.generation == 0 {
		state.generation++
	}
	state.AccountID = account.ID()
	state.accounts[account.ID()] = true
	// This executable segment is request-local: protocol aliases can round-trip,
	// but no persistent root ownership or window-number record may be written.
	record := database.SessionContinuityRecord{AccountID: account.ID(), UpstreamMode: mode, FailoverCount: state.generation, OutboundWindowReset: true}
	epoch := &sessionOutboundEpoch{handler: h, key: hashRiskIdentity(state.key), record: record, temporary: true, owner: responseCacheOwnerForRequest(c, requestAPIKeyID(c)), upstreamAccount: account.EffectiveAccountID()}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), sessionOutboundEpochContextKey{}, epoch))
	usageRequestDiagnosticState(c).UpstreamRoute = &codexRouteDiagnostic{Mode: mode, AccountID: account.ID(), Generation: state.generation}
	return nil
}

func prepareRelaxedAccountOutbound(ctx context.Context, body []byte, headers http.Header) ([]byte, http.Header, error) {
	state := relaxedAccountFallbackFromContext(ctx)
	if state == nil {
		return body, headers, nil
	}
	cleaned, outgoing, report, err := cleanSessionRestartContext(headers, body, nil, state.preserveInput)
	state.Cleanup = report
	if err != nil {
		return nil, nil, err
	}
	return detachForkParentOutbound(cleaned, outgoing, state.references)
}
