package auth

import (
	"testing"
	"time"
)

func TestProbePolicyDefaultOAuthKeepsResetBoundaryAfterRecentAttempt(t *testing.T) {
	now := time.Now()
	a := &Account{AccessToken: "token", Status: StatusReady, UsagePercent5hValid: true, Reset5hAt: now.Add(-time.Minute), UsageUpdatedAt5h: now.Add(-3 * time.Minute), lastAutomaticProbeAt: now.Add(-2 * time.Minute)}
	if !a.NeedsUsageProbe(30*time.Minute) || !a.TryBeginAutomaticProbe(30*time.Minute) {
		t.Fatal("default OAuth reset boundary suppressed by periodic interval")
	}
	a.FinishUsageProbe()
	if a.NeedsUsageProbe(30*time.Minute) || a.TryBeginAutomaticProbe(30*time.Minute) {
		t.Fatal("failed reset-boundary attempt repeated")
	}
}

func TestProbePolicyCustomIntervalRefreshesEvenWithFreshTraffic(t *testing.T) {
	now := time.Now()
	a := &Account{AccessToken: "token", Status: StatusReady, ProbeIntervalMinutes: 1, UsagePercent7dValid: true, UsageUpdatedAt: now, usageObservedAt: now, resetCreditsProbedAt: now, lastAutomaticProbeAt: now.Add(-2 * time.Minute)}
	if !a.NeedsUsageProbe(30 * time.Minute) {
		t.Fatal("custom interval ignored because traffic metadata is fresh")
	}
	if _, ok := a.nextProbeBoundary(now); !ok {
		t.Fatal("custom interval did not arm")
	}
}

func TestProbePolicyProviderOwnedAutoDoesNotSpinStoreBoundary(t *testing.T) {
	for _, provider := range []string{UpstreamGrok, UpstreamAntigravity} {
		a := &Account{UpstreamType: provider, AccessToken: "token", Status: StatusReady, ProbeIntervalMinutes: 1}
		if a.NeedsUsageProbe(time.Minute) {
			t.Fatal("provider-owned auto entered Codex usage loop")
		}
		if due, ok := a.nextProbeBoundary(time.Now()); ok {
			t.Fatalf("unsupported store probe armed %v", due)
		}
	}
}

func TestProbePolicyActiveUnauthorizedDoesNotSpinCustomBoundary(t *testing.T) {
	a := &Account{AccessToken: "token", Status: StatusCooldown, CooldownReason: "unauthorized", CooldownUtil: time.Now().Add(time.Hour), ProbeIntervalMinutes: 1}
	if _, ok := a.nextProbeBoundary(time.Now()); ok {
		t.Fatal("active OAuth unauthorized gate armed a useless custom timer")
	}
}

func TestProbePolicyCustomBoundaryRemainsArmedDuringAttempt(t *testing.T) {
	now := time.Now()
	a := &Account{AccessToken: "token", Status: StatusReady, ProbeMode: "on", ProbeIntervalMinutes: 1, lastAutomaticProbeAt: now, usageProbeInFlight: true}
	due, ok := a.nextProbeBoundary(now)
	if !ok || !due.Equal(now.Add(time.Minute)) {
		t.Fatalf("inflight probe lost subsequent timer: %v %v", due, ok)
	}
}
