package proxy

import (
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// Only account-local dispatch failures authorize a replacement. Request
// validation, authorization of the replacement, replay safety and owner CAS
// remain separate hard gates in the migration transaction.
func sessionAccountFailure(reason string) bool {
	switch reason {
	case "account_missing", "account_disabled", "account_paused", "account_banned",
		"account_error", "credential_unavailable", "account_unauthorized",
		"account_payment_required", "account_usage_exhausted", "account_spark_usage_exhausted",
		"account_cooldown", "model_cooldown", "account_model_unavailable",
		"account_dispatch_limit", "api_key_scope_mismatch", "egress_unavailable",
		"account_session_capacity_full", "upstream_route_disabled",
		"upstream_route_model_unavailable", "upstream_route_requires_bps",
		bpsUploadCooldownReason, "request_excluded":
		return true
	}
	return false
}

func sessionAccountFailoverEnabledBy(reason string) string {
	if !sessionAccountFailure(reason) {
		return ""
	}
	settings := CurrentRuntimeSettings()
	if settings.CodexSessionFailoverEnabled {
		return "session_failover"
	}
	if settings.CodexForkAccountFallbackEnabled {
		return "relaxed_mode"
	}
	return ""
}

// Shared by bound roots, forks and temporary background fallback. A nonempty
// prior route is checked first; changing routes requires the normal identity
// migration transaction and cannot happen through ordinary dispatch.
func (h *Handler) sessionOwnerFailure(c *gin.Context, account *auth.Account, key string, policy auth.DispatchPolicy, info codexRouteRequest, prior string) string {
	if account == nil || account.IsRelayStyle() {
		return ""
	}
	cooldownModel := info.Model
	if passiveInternalRequestAuthorized(c) {
		cooldownModel = "" // Match withRequestModelCooldownFilter's exemption.
	}
	if reason := h.store.SessionDispatchFailure(account, requestAPIKeyID(c), cooldownModel, policy); reason != "" {
		return reason
	}
	mode := selectCodexRoute(account, info.Model, prior, info.Auxiliary)
	if prior != "" {
		mode = normalizedCodexRoute(prior)
		if !account.CodexRouteAllows(mode, "", true) {
			return "upstream_route_disabled"
		}
		if !account.CodexRouteAllows(mode, info.Model, info.Auxiliary) {
			return "upstream_route_model_unavailable"
		}
	} else if mode == "" {
		return "upstream_route_model_unavailable"
	}
	if info.Model != "" && (!info.Auxiliary || !h.passiveInternalModelsAllowed(c)) && !account.SupportsCodexModel(info.Model) {
		return "account_model_unavailable"
	}
	if bpsUploadCooldownForRequest(c.Request.Context(), account, mode) {
		return bpsUploadCooldownReason
	}
	if account.SessionCapacityLimits().Enabled && !h.store.CanAdmitAccountSession(account, key, time.Now(), selectionTraceForRequest(c)) {
		return "account_session_capacity_full"
	}
	return ""
}

func (h *Handler) sessionFailoverReasonForRequest(c *gin.Context, account *auth.Account, key string, policy auth.DispatchPolicy) string {
	ctx := c.Request.Context()
	prior, _ := ctx.Value(codexRouteFloorKey{}).(string)
	if epoch := outboundEpochFromContext(ctx); epoch != nil && account != nil && epoch.record.AccountID == account.ID() {
		if reason := codexRouteFailureForRequest(ctx, account); reason == "upstream_route_requires_bps" {
			return reason
		}
		prior = normalizedCodexRoute(epoch.record.UpstreamMode)
	}
	if reason := h.sessionOwnerFailure(c, account, key, policy, codexRouteRequestInfo(ctx), prior); reason != "" {
		return reason
	}
	if !selectionTraceForRequest(c).SessionModelSupported(account) {
		return "account_model_unavailable"
	}
	return ""
}
