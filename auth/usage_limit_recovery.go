package auth

import "time"

// UsageLimitRecoveryWithDispatch reports the earliest complete usage recovery
// among matching accounts. A zero time means that at least one candidate has
// an unknown reset; callers must not invent a countdown in that case.
// This is an estimate of quota recovery, not a reservation or a guarantee of
// future capacity. Account, model, group, key and egress filters still apply.
func (s *Store) UsageLimitRecoveryWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	now := time.Now()
	filter = s.withUsableEgressFilter(filter)
	var earliest time.Time
	found, unknown := false, false
	for _, account := range s.accountSnapshotAccounts() {
		if account == nil || exclude[account.DBID] || !s.accountAllowedForAPIKey(account, apiKeyID) || (filter != nil && !filter(account)) {
			continue
		}
		recovery, limited := account.dispatchUsageLimitRecovery(policy, now)
		if !limited {
			continue
		}
		found = true
		if recovery.IsZero() {
			unknown = true
		} else if earliest.IsZero() || recovery.Before(earliest) {
			earliest = recovery
		}
	}
	if unknown {
		earliest = time.Time{}
	}
	return earliest, found
}

func (a *Account) dispatchUsageLimitRecovery(policy DispatchPolicy, now time.Time) (time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if policy.IsSpark() {
		if !a.sparkDispatchUsageLimitedLocked(now) {
			return time.Time{}, false
		}
		return a.ResetSparkAt, true
	}
	if a.usageLimitBypassMatchesLocked(policy) {
		if a.freshDispatchUsageLimitedLocked(now) && a.Status == StatusCooldown && a.CooldownUtil.After(now) &&
			isUsageLimitCooldownReason(a.CooldownReason) && a.CooldownReason != premium5hCooldownReason && !a.usageWindowCooldownLocked() {
			return a.CooldownUtil, true
		}
		return time.Time{}, false
	}
	if !a.freshDispatchUsageLimitedLocked(now) {
		return time.Time{}, false
	}
	var recovery time.Time
	unknown := false
	include := func(reset time.Time) {
		if !reset.After(now) {
			unknown = true
		} else if reset.After(recovery) {
			recovery = reset
		}
	}
	// One account must recover all blocking windows, so take its latest reset;
	// the pool above can recover as soon as its earliest account does.
	if !a.creditSkipsUsageWindowLocked() {
		if a.rawPremium5hRateLimitedLocked(now) || quotaAutoPausedByWindow(a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt, a.effectiveAutoPause5h, a.AutoPause5hDisabled, now) {
			include(a.Reset5hAt)
		}
		if a.rawUsageExhaustedLocked() || a.rawUsageWindow7dExhaustedLocked(now) || quotaAutoPausedByWindow(a.UsagePercent7d, a.UsagePercent7dValid, a.Reset7dAt, a.effectiveAutoPause7d, a.AutoPause7dDisabled, now) {
			include(a.Reset7dAt)
		}
	}
	if a.Status == StatusCooldown && a.CooldownUtil.After(now) {
		include(a.CooldownUtil)
	}
	if unknown {
		recovery = time.Time{}
	}
	return recovery, true
}
