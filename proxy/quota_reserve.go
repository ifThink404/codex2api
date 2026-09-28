package proxy

import (
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// QuotaReserveEligibility is a read-only projection of key, model and route
// gates for a shared subscription reserve. It never acquires dispatch capacity,
// claims a session, refreshes credentials, or contacts an inference upstream.
// Request-specific cost/token/concurrency limits remain admission-time checks.
func QuotaReserveEligibility(store *auth.Store, key *database.APIKeyRow, model, mode string) (allowed, available []int64, reason string) {
	allowed, available = []int64{}, []int64{}
	if store == nil || key == nil {
		return allowed, available, "key_unavailable"
	}
	if !key.Enabled || key.ExpiresAt.Valid && !key.ExpiresAt.Time.After(time.Now()) || key.QuotaLimit > 0 && key.QuotaUsed >= key.QuotaLimit {
		return allowed, available, "key_unavailable"
	}
	if checkAPIKeyModel(model, key.Limits) != "" {
		return allowed, available, "key_model_not_allowed"
	}
	if strings.Contains(strings.ToLower(model), "spark") {
		return allowed, available, "separate_quota_window"
	}
	h := &Handler{store: store}
	for _, account := range store.Accounts() {
		if account.IsRelayStyle() || account.IsCodexAgentIdentity() || !account.AllowsAPIKey(key.ID) || !store.APIKeyAllowsAccount(key.ID, account) {
			continue
		}
		mapped, _ := h.resolveConfiguredRequestModel(model, account.CodexModels())
		if !account.SupportsCodexModel(mapped) {
			continue
		}
		routeAllowed := account.CodexRouteAllows(mode, mapped, false)
		if mode == "" {
			routeAllowed = account.CodexRouteAllows("native", mapped, false) || account.CodexRouteAllows("bps", mapped, false)
		}
		if !routeAllowed {
			continue
		}
		allowed = append(allowed, account.DBID)
		if store.SessionDispatchFailure(account, key.ID, mapped, auth.DispatchPolicyStandard.WithModel(mapped)) == "" {
			available = append(available, account.DBID)
		}
	}
	return allowed, available, ""
}
