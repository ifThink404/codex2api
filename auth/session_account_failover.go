package auth

import (
	"sync/atomic"
	"time"
)

func (account *Account) SessionAccountFailoverReason() string {
	if account == nil || account.IsRelayStyle() {
		return ""
	}
	if atomic.LoadInt32(&account.Disabled) != 0 {
		return "account_disabled"
	}
	if atomic.LoadInt32(&account.DispatchPaused) != 0 {
		return "account_paused"
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	now := time.Now()
	switch {
	case account.healthTierLocked() == HealthTierBanned:
		return "account_banned"
	case account.Status == StatusError:
		return "account_error"
	case !account.hasDispatchCredentialLocked():
		return "credential_unavailable"
	case account.usageWindowBlocksFreshDispatchLocked(now), account.quotaAutoPausedLocked(now):
		return "account_usage_exhausted"
	case account.Status == StatusCooldown && now.Before(account.CooldownUtil):
		if isUsageLimitCooldownReason(account.CooldownReason) {
			return "account_usage_exhausted"
		}
		if account.CooldownReason == "unauthorized" {
			return "account_unauthorized"
		}
		if account.CooldownReason == "payment_required" {
			return "account_payment_required"
		}
		return "account_cooldown"
	}
	return ""
}

// SessionDispatchFailure uses the same cached state, authorization and egress
// gates as the scheduler. It does not acquire a slot or mutate session ownership.
// Quota bypass and lazy refresh keep their normal dispatch semantics.
func (store *Store) SessionDispatchFailure(account *Account, apiKeyID int64, model string, policy DispatchPolicy) string {
	if account == nil {
		return "account_missing"
	}
	if account.IsRelayStyle() {
		return ""
	}
	localFailure := func() string {
		if account.dispatchUsageOverrideEligible(policy) {
			return ""
		}
		if policy.IsSpark() && account.SparkDispatchUsageLimited() {
			return "account_spark_usage_exhausted"
		}
		reason := account.SessionAccountFailoverReason()
		if reason == "credential_unavailable" && store.GetLazyMode() && store.accountLazySelectable(account) {
			reason = ""
		}
		return reason
	}
	// As in takeByIDModeWithCapacity, reject local terminal state before cache
	// hydration, which otherwise overwrites StatusError with StatusCooldown.
	if reason := localFailure(); reason != "" {
		return reason
	}
	store.accountHasCachedCooldown(account)
	if reason := localFailure(); reason != "" {
		return reason
	}
	if !store.APIKeyAllowsAccount(apiKeyID, account) {
		return "api_key_scope_mismatch"
	}
	if store.accountHasCachedModelCooldown(account, model) {
		return "model_cooldown"
	}
	if !store.accountHasUsableEgress(account) {
		return "egress_unavailable"
	}
	if account.GetDispatchCountSnapshot().Limited {
		return "account_dispatch_limit"
	}
	return ""
}
