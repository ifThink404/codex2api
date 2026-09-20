package auth

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

type probeContextKey uint8

const (
	automaticProbeReservationKey probeContextKey = iota
	manualProbeKey
)

func WithAutomaticProbeReservation(ctx context.Context, account *Account) context.Context {
	return context.WithValue(ctx, automaticProbeReservationKey, account)
}

func HasAutomaticProbeReservation(ctx context.Context, account *Account) bool {
	return ctx.Value(automaticProbeReservationKey) == account
}

func WithManualProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, manualProbeKey, true)
}

func IsManualProbe(ctx context.Context) bool {
	manual, _ := ctx.Value(manualProbeKey).(bool)
	return manual
}

const (
	ProbeModeAuto                = "auto"
	ProbeModeOff                 = "off"
	ProbeModeOn                  = "on"
	ProbeModeCredentialKey       = "probe_mode"
	ProbeIntervalCredentialKey   = "probe_interval_minutes"
	APIAutoRecoveryCredentialKey = "api_auto_recovery_enabled"
)

// IsAPIKeyAccount identifies native API-key credentials, independently of the
// provider's relay routing. OAuth, SSO and setup tokens are not API keys.
func (a *Account) IsAPIKeyAccount() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isAPIKeyAccountLocked()
}

// isAPIKeyAccountLocked requires a.mu to be held by the caller.
func (a *Account) isAPIKeyAccountLocked() bool {
	if a == nil {
		return false
	}
	return a.isOpenAIResponsesAPILocked() || a.isClaudeAPIKeyLocked() ||
		((a.isGrokAPILocked() || a.isAntigravityAPILocked()) && strings.TrimSpace(a.APIKey) != "")
}

// APIAutoRecoveryEnabledForAccount requires explicit operator opt-in. Missing
// credentials retain historical authentication-error behavior.
func (a *Account) APIAutoRecoveryEnabledForAccount() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.apiAutoRecoveryEnabledLocked()
}

func (a *Account) apiAutoRecoveryEnabledLocked() bool {
	return a != nil && a.APIAutoRecoveryEnabled && a.isAPIKeyAccountLocked()
}

func (a *Account) GetAPIAutoRecoveryEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.APIAutoRecoveryEnabled
}

func NormalizeProbeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ProbeModeOff:
		return ProbeModeOff
	case ProbeModeOn:
		return ProbeModeOn
	default:
		return ProbeModeAuto
	}
}

func NormalizeProbeIntervalMinutes(minutes int64) int {
	if minutes < 1 || minutes > 1440 {
		return 0
	}
	return int(minutes)
}

func (a *Account) automaticProbesEnabledLocked() bool {
	mode := NormalizeProbeMode(a.ProbeMode)
	return mode == ProbeModeOn || (mode == ProbeModeAuto && !a.isAPIKeyAccountLocked())
}

// AutomaticProbesEnabled controls optional usage, recovery and connectivity
// requests. Token renewal and read-only control-plane sync are independent.
// Missing or stale observations are never evidence of account unavailability.
func (a *Account) AutomaticProbesEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.automaticProbesEnabledLocked()
}

func (a *Account) probeIntervalLocked(inherited time.Duration) time.Duration {
	if minutes := NormalizeProbeIntervalMinutes(int64(a.ProbeIntervalMinutes)); minutes > 0 {
		return time.Duration(minutes) * time.Minute
	}
	if inherited <= 0 {
		return defaultUsageProbeMaxAge
	}
	return inherited
}

func (a *Account) ProbeInterval(inherited time.Duration) time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.probeIntervalLocked(inherited)
}

func (a *Account) automaticProbeDueLocked(now time.Time, inherited time.Duration) bool {
	if !a.automaticProbesEnabledLocked() || a.usageProbeInFlight || a.recoveryProbeInFlight {
		return false
	}
	if a.lastAutomaticProbeAt.IsZero() || now.Sub(a.lastAutomaticProbeAt) >= a.probeIntervalLocked(inherited) {
		return true
	}
	// Preserve the inherited OAuth reset/cooldown boundary behavior. A boundary
	// crossed since the last attempt gets one sample; a failed attempt after
	// that boundary still waits for the ordinary interval before retrying.
	if NormalizeProbeMode(a.ProbeMode) != ProbeModeAuto || a.ProbeIntervalMinutes != 0 {
		return false
	}
	crossed := func(boundary time.Time) bool { return boundary.After(a.lastAutomaticProbeAt) && !boundary.After(now) }
	return (a.UsagePercent5hValid && a.UsageUpdatedAt5h.Before(a.Reset5hAt) && crossed(a.Reset5hAt)) ||
		(a.UsagePercent7dValid && a.UsageUpdatedAt.Before(a.Reset7dAt) && crossed(a.Reset7dAt)) ||
		(a.Status == StatusCooldown && a.CooldownReason != "unauthorized" && !a.isTransientRateLimitCooldownLocked() && crossed(a.CooldownUtil))
}

// TryBeginAutomaticProbe atomically reserves an attempt, including failed
// attempts. FinishUsageProbe releases it. Explicit Test uses its manual path.
func (a *Account) TryBeginAutomaticProbe(inherited time.Duration) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if !a.automaticProbeDueLocked(now, inherited) {
		return false
	}
	a.usageProbeInFlight = true
	a.lastAutomaticProbeAt = now
	return true
}

func (a *Account) GetProbePolicy() (string, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeProbeMode(a.ProbeMode), NormalizeProbeIntervalMinutes(int64(a.ProbeIntervalMinutes))
}

func (a *Account) setProbePolicyLocked(mode string, interval int) {
	mode = NormalizeProbeMode(mode)
	interval = NormalizeProbeIntervalMinutes(int64(interval))
	if NormalizeProbeMode(a.ProbeMode) != mode || a.ProbeIntervalMinutes != interval {
		a.lastAutomaticProbeAt = time.Time{}
	}
	a.ProbeMode, a.ProbeIntervalMinutes = mode, interval
}

func (a *Account) setProbePolicyFromRowLocked(row *database.AccountRow) {
	a.setProbePolicyLocked(row.GetCredential(ProbeModeCredentialKey), ProbeIntervalMinutesFromRow(row))
}

func ProbeIntervalMinutesFromRow(row *database.AccountRow) int {
	minutes, _ := row.GetCredentialInt64(ProbeIntervalCredentialKey)
	return NormalizeProbeIntervalMinutes(minutes)
}

// ApplyAccountProbePolicyPatch publishes committed credentials immediately.
func (s *Store) ApplyAccountProbePolicyPatch(id int64, updates map[string]interface{}) {
	a := s.FindByID(id)
	if a == nil {
		return
	}
	mode, modeSet := updates[ProbeModeCredentialKey].(string)
	interval, intervalSet := updates[ProbeIntervalCredentialKey].(int)
	recovery, recoverySet := updates[APIAutoRecoveryCredentialKey].(bool)
	if !modeSet && !intervalSet && !recoverySet {
		return
	}
	a.mu.Lock()
	if recoverySet {
		a.APIAutoRecoveryEnabled = recovery
	}
	recoveryChanged := a.enforceAPIAutoRecoveryOptOutLocked(time.Now())
	if recoveryChanged {
		a.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	}
	if !modeSet {
		mode = a.ProbeMode
	}
	if !intervalSet {
		interval = a.ProbeIntervalMinutes
	}
	a.setProbePolicyLocked(mode, interval)
	a.mu.Unlock()
	if recoveryChanged {
		s.fastSchedulerUpdate(a)
	}
	s.WakeBoundaryProbe(time.Time{})
}
