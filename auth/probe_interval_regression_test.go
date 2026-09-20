package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbePolicyRecoveryUsesConfiguredInterval(t *testing.T) {
	a := &Account{AccessToken: "token", RefreshToken: "refresh", Status: StatusReady, HealthTier: HealthTierBanned, lastAutomaticProbeAt: time.Now().Add(-11 * time.Minute), LastRecoveryProbeAt: time.Now().Add(-11 * time.Minute)}
	if !a.NeedsRecoveryProbe(10 * time.Minute) {
		t.Fatal("configured recovery not due")
	}
	if !a.TryBeginRecoveryProbe(10 * time.Minute) {
		t.Fatal("recovery reservation ignored configured ten minutes")
	}
	a.FinishRecoveryProbe()
}

func TestProbePolicyTerminalAndMissingCredentialDoNotArmBoundary(t *testing.T) {
	for _, a := range []*Account{
		{ProbeMode: "on"}, {ProbeMode: "on", AccessToken: "token", Status: StatusError},
		{ProbeMode: "auto", ProbeIntervalMinutes: 1}, {ProbeMode: "auto", ProbeIntervalMinutes: 1, AccessToken: "token", Status: StatusError},
	} {
		if due, ok := a.nextProbeBoundary(time.Now()); ok {
			t.Fatalf("unschedulable account armed %v", due)
		}
		if a.NeedsUsageProbe(time.Minute) {
			t.Fatal("unschedulable account requested automatic probe")
		}
	}
}

func TestProbePolicyExplicitForceBypassesOffAndInterval(t *testing.T) {
	s := NewStore(nil, nil, nil)
	defer s.Stop()
	a := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, ProbeMode: "off", lastAutomaticProbeAt: time.Now()}
	s.AddAccount(a)
	var calls atomic.Int32
	s.SetUsageProbeFunc(func(context.Context, *Account) error { calls.Add(1); return nil })
	s.parallelProbeUsageWith(context.Background(), 0)
	if calls.Load() != 1 {
		t.Fatalf("manual force calls=%d want 1", calls.Load())
	}
}
