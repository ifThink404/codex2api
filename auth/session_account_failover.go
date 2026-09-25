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
	}
	return ""
}
