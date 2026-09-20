package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbePolicyModesAndFailedAttempts(t *testing.T) {
	for _, upstream := range []string{"", UpstreamOpenAIResponses, UpstreamClaude, UpstreamGrok, UpstreamAntigravity} {
		for _, mode := range []string{"auto", "off", "on"} {
			t.Run(upstream+"/"+mode, func(t *testing.T) {
				a := &Account{UpstreamType: upstream, AccessToken: "token", RefreshToken: "refresh", APIKey: "key", BaseURL: "https://example.test", Status: StatusReady, ProbeMode: mode, ProbeIntervalMinutes: 7}
				if upstream == UpstreamClaude {
					a.ClaudeAuthKind = ClaudeAuthKindAPIKey
				}
				want := mode == "on" || (mode == "auto" && upstream == "")
				if got := a.AutomaticProbesEnabled(); got != want {
					t.Fatalf("automatic enabled=%v want %v", got, want)
				}
				if got := a.NeedsUsageProbe(time.Minute); got != want {
					t.Fatalf("NeedsUsageProbe=%v want %v", got, want)
				}
				if got := a.TryBeginAutomaticProbe(time.Minute); got != want {
					t.Fatalf("begin=%v want %v", got, want)
				}
				if want {
					a.FinishUsageProbe() // no success recorded: failures must still throttle
					if a.NeedsUsageProbe(time.Minute) || a.TryBeginAutomaticProbe(time.Minute) {
						t.Fatal("failed attempt was not throttled")
					}
					a.mu.Lock()
					a.lastAutomaticProbeAt = time.Now().Add(-7*time.Minute - time.Second)
					a.mu.Unlock()
					if !a.TryBeginAutomaticProbe(time.Minute) {
						t.Fatal("probe did not become eligible after interval")
					}
					a.FinishUsageProbe()
				}
			})
		}
	}
}

func TestProbePolicyOffBlocksAllStoreAutomaticPaths(t *testing.T) {
	s := NewStore(nil, nil, nil)
	defer s.Stop()
	a := &Account{DBID: 1, AccessToken: "token", RefreshToken: "refresh", Status: StatusReady, ProbeMode: "off", Reset5hAt: time.Now().Add(time.Minute), UsagePercent5hValid: true}
	s.AddAccount(a)
	var calls atomic.Int32
	s.SetUsageProbeFunc(func(context.Context, *Account) error { calls.Add(1); return errors.New("probe failed") })
	s.parallelProbeUsage(context.Background())
	s.TriggerUsageProbeForAccountAsync(a)
	s.VerifyAccountAuthAsync(a)
	if _, ok := a.nextProbeBoundary(time.Now()); ok {
		t.Fatal("off scheduled a boundary probe")
	}
	a.mu.Lock()
	a.HealthTier = HealthTierBanned
	a.mu.Unlock()
	if a.NeedsRecoveryProbe(time.Minute) || a.TryBeginRecoveryProbe() {
		t.Fatal("off allowed recovery probe")
	}
	s.parallelRecoveryProbe(context.Background())
	s.wg.Wait()
	if calls.Load() != 0 {
		t.Fatalf("off ran %d automatic callbacks", calls.Load())
	}
	if !a.TryBeginUsageProbe() {
		t.Fatal("explicit/manual probe lock must remain usable when off")
	}
	a.FinishUsageProbe()
}

func TestProbePolicyFailedSweepThrottlesAndSuccessCanRecord(t *testing.T) {
	s := NewStore(nil, nil, nil)
	defer s.Stop()
	a := &Account{DBID: 1, UpstreamType: UpstreamOpenAIResponses, BaseURL: "https://example.test", APIKey: "key", Status: StatusReady, ProbeMode: "on", ProbeIntervalMinutes: 1}
	s.AddAccount(a)
	var calls atomic.Int32
	s.SetUsageProbeFunc(func(context.Context, *Account) error { calls.Add(1); return errors.New("upstream failure") })
	s.parallelProbeUsage(context.Background())
	s.parallelProbeUsage(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("got %d attempts, want exactly one failed attempt", calls.Load())
	}
	a.mu.Lock()
	a.lastAutomaticProbeAt = time.Now().Add(-2 * time.Minute)
	a.mu.Unlock()
	s.SetUsageProbeFunc(func(_ context.Context, acc *Account) error {
		calls.Add(1)
		s.ReportRequestSuccess(acc, time.Millisecond)
		return nil
	})
	s.parallelProbeUsage(context.Background())
	if calls.Load() != 2 || a.LastSuccessAt.IsZero() {
		t.Fatal("due native probe did not record success")
	}
}
