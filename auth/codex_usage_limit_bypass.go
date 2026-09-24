package auth

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

const (
	CodexUsageLimitBypassEnabledKey = "codex_usage_limit_bypass_enabled"
	CodexUsageLimitBypassModelsKey  = "codex_usage_limit_bypass_models"
)

func ValidateCodexUsageLimitBypassModels(models []string) error {
	for _, model := range models {
		if strings.Contains(model, "*") {
			return fmt.Errorf("指定模型必须填写完整模型 ID，不支持通配符：%q", model)
		}
	}
	return ValidateCodexRouteModels(models)
}

func (a *Account) hasUsageLimitBypassLocked() bool {
	kind := strings.ToLower(strings.TrimSpace(a.UpstreamType))
	if (kind != "" && kind != "codex") || a.isRelayStyleLocked() || !a.CodexUsageLimitBypassEnabled {
		return false
	}
	for _, model := range a.CodexUsageLimitBypassModels {
		if strings.TrimSpace(model) != "" && !strings.Contains(model, "*") {
			return true
		}
	}
	return false
}

func (a *Account) usageLimitBypassMatchesLocked(policy DispatchPolicy) bool {
	if policy.IsSpark() || !a.hasUsageLimitBypassLocked() || policy.model == "" {
		return false
	}
	for _, model := range a.CodexUsageLimitBypassModels {
		if !strings.Contains(model, "*") && strings.EqualFold(strings.TrimSpace(model), policy.model) {
			return true
		}
	}
	return false
}

func (a *Account) UsageLimitBypassMatches(policy DispatchPolicy) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usageLimitBypassMatchesLocked(policy)
}

func (a *Account) usageLimitBypassEligibleLocked(now time.Time) bool {
	if atomic.LoadInt32(&a.Disabled) != 0 || atomic.LoadInt32(&a.DispatchPaused) != 0 || a.Status == StatusError || a.healthTierLocked() == HealthTierBanned || !a.hasDispatchCredentialLocked() {
		return false
	}
	// Only window cooldowns are exempt. Actual request throttling, overload,
	// authentication errors and manual pauses remain authoritative.
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) && a.CooldownReason != premium5hCooldownReason && !a.usageWindowCooldownLocked() {
		return false
	}
	return true
}

func (a *Account) UsageLimitBypassEligible(policy DispatchPolicy) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usageLimitBypassMatchesLocked(policy) && a.usageLimitBypassEligibleLocked(time.Now())
}

// Quota-only housekeeping must not delete an account which can still serve
// configured models. Explicit/manual deletion and genuine errors stay separate.
func (a *Account) canServeUsageLimitBypassModels() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.hasUsageLimitBypassLocked() && a.usageLimitBypassEligibleLocked(time.Now())
}

func (a *Account) dispatchUsageOverrideEligible(policy DispatchPolicy) bool {
	if policy.IsSpark() {
		return a.SparkDispatchEligible()
	}
	return a.UsageLimitBypassEligible(policy)
}

func (a *Account) fastSchedulerSnapshotForUsageBypass(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	tier := a.healthTierLocked()
	score := a.DispatchScore
	if score == 0 {
		score = a.SchedulerScore
	}
	limit := concurrencyLimitForTier(a.effectiveBaseConcurrencyLocked(baseLimit), tier)
	return tier, score, limit, atomic.LoadInt64(&a.TotalRequests) > 10, a.usageLimitBypassEligibleLocked(now)
}

func (s *Store) ApplyAccountUsageLimitBypass(id int64, enabled database.OptionalBool, models database.OptionalStringSlice) {
	a := s.FindByID(id)
	if a == nil {
		return
	}
	a.mu.Lock()
	if enabled.Set {
		a.CodexUsageLimitBypassEnabled = enabled.Value
	}
	if models.Set {
		a.CodexUsageLimitBypassModels = append([]string(nil), models.Values...)
	}
	a.mu.Unlock()
	s.fastSchedulerUpdate(a)
}
