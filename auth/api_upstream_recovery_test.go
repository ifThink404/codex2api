package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/cache"
)

func newAPIRecoveryTestAccount(id int64) *Account {
	a := newFastSchedulerTestAccount(id, HealthTierHealthy, 100, 4)
	a.AccessToken = ""
	a.UpstreamType, a.BaseURL, a.APIKey, a.PlanType = UpstreamOpenAIResponses, "https://relay.invalid", "test-key", "api"
	a.APIAutoRecoveryEnabled = true
	return a
}

func TestAPIUnauthorizedIsRecoverableWithoutProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := newAPIRecoveryTestAccount(1)
	s := &Store{accounts: []*Account{a}, maxConcurrency: 4, backgroundCtx: ctx}
	s.rebuildAccountIndex()
	s.SetSchedulerEngine("indexed")
	s.ReportRequestFailure(a, "unauthorized", time.Millisecond)
	s.MarkCooldownWithError(a, 24*time.Hour, "unauthorized", "auth_unavailable: upstream OAuth token invalidated")
	if a.IsBanned() || atomic.LoadInt32(&a.Disabled) != 0 {
		t.Fatal("API upstream auth error permanently bans the gateway's API account")
	}
	if a.IsAvailable() || time.Until(a.CooldownUtil) > 5*time.Minute {
		t.Fatalf("expected bounded cooldown, got %s until %s", a.CooldownReason, a.CooldownUtil)
	}
	// Expire the actual timer used by the indexed scheduler, without making a
	// test take fifteen seconds or issuing a background/manual network probe.
	a.mu.Lock()
	a.CooldownUtil = time.Now().Add(20 * time.Millisecond)
	a.transientRateLimitUntil = a.CooldownUtil
	a.armTransientRateLimitRecoveryLocked(s)
	a.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if picked := s.getFastScheduler().Acquire(); picked != nil {
			s.getFastScheduler().Release(picked)
			s.ReportRequestSuccess(a, time.Millisecond)
			if !a.IsAvailable() || a.GetCooldownReason() != "" || a.ErrorMsg != "" {
				t.Fatal("successful business request did not restore API health")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("API account never re-entered scheduler after cooldown without probe")
}

func TestAPIRecoveryPreservesNewerCooldownAndManualPause(t *testing.T) {
	a := newAPIRecoveryTestAccount(1)
	s := &Store{maxConcurrency: 4}
	s.MarkCooldownWithError(a, time.Minute, "unauthorized", "upstream auth unavailable")
	s.ReportRequestSuccess(a, time.Millisecond)
	if a.IsAvailable() || a.GetCooldownReason() == "" {
		t.Fatal("an in-flight success erased a newer cooldown")
	}
	s.MarkCooldown(a, time.Hour, "usage_limit")
	s.MarkCooldownWithError(a, time.Minute, "unauthorized", "upstream auth unavailable")
	if a.GetCooldownReason() != "usage_limit" {
		t.Fatal("temporary upstream auth error replaced a stronger quota cooldown")
	}
	atomic.StoreInt32(&a.DispatchPaused, 1)
	s.ReportRequestSuccess(a, time.Millisecond)
	if a.IsAvailable() || atomic.LoadInt32(&a.DispatchPaused) != 1 {
		t.Fatal("success re-enabled an operator-paused account")
	}
}

func TestAPIRecoveryCooldownAcrossInstances(t *testing.T) {
	c := cache.NewMemory(1)
	defer c.Close()
	a, b := newAPIRecoveryTestAccount(1), newAPIRecoveryTestAccount(1)
	s1 := &Store{accounts: []*Account{a}, maxConcurrency: 4, tokenCache: c}
	s2 := &Store{accounts: []*Account{b}, maxConcurrency: 4, tokenCache: c}
	s1.ReportRequestFailure(a, "unauthorized", time.Millisecond)
	s1.MarkCooldownWithError(a, time.Minute, "unauthorized", "upstream error")
	if !s2.accountHasCachedCooldown(b) {
		t.Fatal("API backoff was not shared")
	}
	if b.IsBanned() || !b.isTransientRateLimitCooldownLocked() {
		t.Fatal("shared API backoff became a permanent OAuth ban")
	}
}

func TestOAuthUnauthorizedStillRequiresRecovery(t *testing.T) {
	a := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	s := &Store{maxConcurrency: 4}
	s.ReportRequestFailure(a, "unauthorized", time.Millisecond)
	s.MarkCooldownWithError(a, time.Minute, "unauthorized", "token expired")
	if !a.IsBanned() || time.Until(a.CooldownUtil) < 5*time.Hour {
		t.Fatal("OAuth unauthorized protection changed")
	}
}

func TestAPIUnauthorizedRecoveryAppliesToAllKeyProviders(t *testing.T) {
	for _, a := range []*Account{
		newAPIRecoveryTestAccount(1),
		{DBID: 2, UpstreamType: UpstreamGrok, APIKey: "test", Status: StatusReady, APIAutoRecoveryEnabled: true},
		{DBID: 3, UpstreamType: UpstreamAntigravity, APIKey: "test", Status: StatusReady, APIAutoRecoveryEnabled: true},
		{DBID: 4, UpstreamType: UpstreamClaude, AccessToken: "sk-ant-api-test", ClaudeAuthKind: ClaudeAuthKindAPIKey, Status: StatusReady, APIAutoRecoveryEnabled: true},
	} {
		t.Run(a.UpstreamType, func(t *testing.T) {
			s := &Store{maxConcurrency: 4}
			s.ReportRequestFailure(a, "unauthorized", time.Millisecond)
			s.MarkCooldownWithError(a, time.Hour, "unauthorized", "credential rejected")
			if a.IsBanned() || a.GetCooldownReason() != APIUpstreamUnavailableCooldownReason {
				t.Fatalf("API key incorrectly needs OAuth recovery: tier=%s reason=%s", a.HealthTier, a.GetCooldownReason())
			}
		})
	}
}

func TestAPIRecoverySwitchOffPreservesLegacyBan(t *testing.T) {
	a := newAPIRecoveryTestAccount(1)
	a.APIAutoRecoveryEnabled = false
	s := &Store{maxConcurrency: 4}
	s.ReportRequestFailure(a, "unauthorized", time.Millisecond)
	s.MarkCooldownWithError(a, time.Minute, "unauthorized", "token rejected")
	if !a.IsBanned() || time.Until(a.CooldownUtil) < 5*time.Hour {
		t.Fatal("disabled API recovery switch did not preserve existing protection")
	}
}

func TestAPIRecoveryCachedFailureTimeControlsStableReset(t *testing.T) {
	for _, prior := range []string{"zero", "old", "newer"} {
		t.Run(prior, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := newAPIRecoveryTestAccount(1)
				s := &Store{maxConcurrency: 4}
				observed := time.Now().Add(-10 * time.Second)
				want := observed
				switch prior {
				case "old":
					a.LastRateLimitedAt = time.Now().Add(-time.Hour)
					a.LastFailureAt = a.LastRateLimitedAt
				case "newer":
					want = time.Now().Add(-time.Second)
					a.LastRateLimitedAt, a.LastFailureAt = want, want
				}
				s.applyCachedAccountCooldown(a, runtimeCooldownRecord{
					Kind: cache.CooldownKindTransient, Reason: APIUpstreamUnavailableCooldownReason,
					ResetAt: time.Now().Add(15 * time.Second), UpdatedAt: observed, BackoffLevel: 3,
				})
				if !a.LastRateLimitedAt.Equal(want) || !a.LastFailureAt.Equal(want) {
					t.Errorf("shared failure time not preserved monotonically: rate=%v failure=%v want=%v", a.LastRateLimitedAt, a.LastFailureAt, want)
				}
				time.Sleep(16 * time.Second)
				s.ReportRequestSuccess(a, time.Millisecond)
				if got := a.TransientRateLimitBackoff(); got != 3 {
					t.Errorf("early business success reset imported backoff: got %d, want 3", got)
				}
				time.Sleep(5 * time.Minute)
				s.ReportRequestSuccess(a, time.Millisecond)
				if got := a.TransientRateLimitBackoff(); got != 0 {
					t.Errorf("stable business success did not reset imported backoff: got %d", got)
				}
			})
		})
	}
}

func TestAPIRecoveryOptOutIgnoresStaleSharedRecords(t *testing.T) {
	for _, kind := range []string{cache.CooldownKindTransient, ""} {
		t.Run("kind="+kind, func(t *testing.T) {
			c := cache.NewMemory(1)
			defer c.Close()
			a := newAPIRecoveryTestAccount(1)
			a.APIAutoRecoveryEnabled = false
			s := &Store{accounts: []*Account{a}, maxConcurrency: 4, tokenCache: c}
			record := runtimeCooldownRecord{Kind: kind, Reason: APIUpstreamUnavailableCooldownReason,
				ResetAt: time.Now().Add(time.Minute), UpdatedAt: time.Now(), BackoffLevel: 2}
			s.cacheAccountCooldownRecord(a.DBID, record)
			s.applyCachedAccountCooldown(a, record)
			if !a.IsAvailable() || a.GetCooldownReason() != "" {
				t.Error("stale API recovery import fenced an opted-out ready account")
			}
			if s.accountHasCachedCooldown(a) {
				t.Error("ignored stale API record still blocks account selection")
			}
			if _, ok := s.getCachedAccountCooldown(a.DBID); !ok {
				t.Error("local opt-out deleted a shared record still needed by other instances")
			}
		})
	}
}

func TestAPIRecoveryOptOutStillImportsOrdinary429(t *testing.T) {
	c := cache.NewMemory(1)
	defer c.Close()
	a := newAPIRecoveryTestAccount(1)
	a.APIAutoRecoveryEnabled = false
	s := &Store{accounts: []*Account{a}, maxConcurrency: 4, tokenCache: c}
	s.cacheAccountCooldownRecord(a.DBID, runtimeCooldownRecord{
		Kind: cache.CooldownKindTransient, Reason: ResponsesRateLimitedCooldownReason,
		ResetAt: time.Now().Add(time.Minute), UpdatedAt: time.Now(), BackoffLevel: 2,
	})
	if !s.accountHasCachedCooldown(a) || a.IsAvailable() || a.IsBanned() {
		t.Fatal("API recovery opt-out changed ordinary shared 429 throttling")
	}
}

func TestAPIRecoveryOptOutTimerRestoresLegacyAuthGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a := newAPIRecoveryTestAccount(1)
		s := &Store{accounts: []*Account{a}, maxConcurrency: 4, backgroundCtx: ctx}
		s.rebuildAccountIndex()
		s.SetSchedulerEngine("indexed")
		s.MarkAPIUpstreamUnavailable(a, 0, "upstream auth rejected")
		a.mu.Lock()
		a.APIAutoRecoveryEnabled = false
		a.mu.Unlock()
		time.Sleep(16 * time.Second)
		synctest.Wait()
		if !a.IsBanned() || a.GetCooldownReason() != "unauthorized" {
			t.Errorf("opted-out pending recovery escaped the legacy auth gate: banned=%v reason=%s", a.IsBanned(), a.GetCooldownReason())
		}
		if picked := s.getFastScheduler().Acquire(); picked != nil {
			s.getFastScheduler().Release(picked)
			t.Error("expired API recovery re-entered indexed scheduler after opt-out")
		}
		s.ReportRequestSuccess(a, time.Millisecond)
		if a.IsAvailable() {
			t.Error("late business success erased the opt-out auth gate")
		}
	})
}

func TestAPIRecoveryOptOutLateSuccessCannotRestorePendingWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newAPIRecoveryTestAccount(1)
		s := &Store{maxConcurrency: 4}
		s.MarkAPIUpstreamUnavailable(a, 0, "upstream auth rejected")
		a.APIAutoRecoveryEnabled = false
		time.Sleep(16 * time.Second)
		s.ReportRequestSuccess(a, time.Millisecond)
		if a.IsAvailable() || !a.IsBanned() || a.GetCooldownReason() != "unauthorized" {
			t.Fatal("late success made an opted-out pending recovery dispatchable")
		}
	})
}

func TestAPIRecoveryOptOutRejectsPreparedTemporaryFailure(t *testing.T) {
	a := newAPIRecoveryTestAccount(1)
	s := &Store{maxConcurrency: 4}
	// The caller selected API recovery, then a policy update won the account lock.
	a.APIAutoRecoveryEnabled = false
	s.markTransientUpstreamCooldown(a, 0, APIUpstreamUnavailableCooldownReason, "late rejection")
	if a.GetCooldownReason() == APIUpstreamUnavailableCooldownReason {
		t.Fatal("a prepared failure installed API recovery after opt-out")
	}
}

func TestAPIRecoveryExactAuthCooldownRemainsAuthoritative(t *testing.T) {
	for _, reason := range []string{"unauthorized", "forbidden"} {
		t.Run(reason, func(t *testing.T) {
			a := newAPIRecoveryTestAccount(1)
			s := &Store{maxConcurrency: 4}
			s.MarkCooldownWithErrorExactDuration(a, 24*time.Hour, reason, "permanent provider denial")
			s.ReportRequestSuccess(a, time.Millisecond)
			if got, until := a.GetCooldownSnapshot(); got != reason || time.Until(until) < 23*time.Hour {
				t.Fatalf("exact auth gate was remapped to temporary recovery: reason=%s until=%v", got, until)
			}
			if a.IsAvailable() || (reason == "unauthorized" && !a.IsBanned()) {
				t.Fatal("late success bypassed exact auth gate")
			}
		})
	}
}

func TestAPIRecoveryOptOutHookKeepsLegacyGateAfterTimerDeadline(t *testing.T) {
	for _, recentAuthFailure := range []bool{false, true} {
		name := "first auth failure"
		if recentAuthFailure {
			name = "recent auth failure"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				a := newAPIRecoveryTestAccount(1)
				s := &Store{accounts: []*Account{a}, maxConcurrency: 4, backgroundCtx: ctx}
				s.rebuildAccountIndex()
				s.SetSchedulerEngine("indexed")
				s.MarkAPIUpstreamUnavailable(a, 0, "upstream auth rejected")
				atomic.StoreInt32(&a.DispatchPaused, 1)
				wantDuration := 6 * time.Hour
				if recentAuthFailure {
					a.LastUnauthorizedAt = time.Now().Add(-time.Hour)
					wantDuration = 24 * time.Hour
				}
				now := time.Now()
				a.mu.Lock()
				a.APIAutoRecoveryEnabled = false
				changed := a.enforceAPIAutoRecoveryOptOutLocked(now)
				a.recomputeSchedulerLocked(4)
				stopped := a.transientRateLimitTimer == nil && a.transientRateLimitUntil.IsZero()
				a.mu.Unlock()
				s.fastSchedulerUpdate(a)
				if !changed || !stopped || !a.IsBanned() {
					t.Fatal("policy hook did not cancel pending recovery and establish legacy ban")
				}
				if reason, until := a.GetCooldownSnapshot(); reason != "unauthorized" || !until.Equal(now.Add(wantDuration)) {
					t.Fatalf("wrong legacy auth gate: %s until %v", reason, until)
				}
				if a.ErrorMsg != "upstream auth rejected" || atomic.LoadInt32(&a.DispatchPaused) != 1 {
					t.Fatal("policy transition erased failure details or manual pause")
				}
				// Removing the independent manual fence must not revive the auth gate.
				atomic.StoreInt32(&a.DispatchPaused, 0)
				time.Sleep(16 * time.Second)
				synctest.Wait()
				a.mu.Lock()
				again := a.enforceAPIAutoRecoveryOptOutLocked(time.Now())
				a.mu.Unlock()
				if again || !a.CooldownUtil.Equal(now.Add(wantDuration)) {
					t.Fatal("repeated policy publication extended the existing legacy gate")
				}
				if picked := s.getFastScheduler().Acquire(); picked != nil {
					s.getFastScheduler().Release(picked)
					t.Fatal("cancelled transient timer reopened the indexed account")
				}
			})
		})
	}
}

func TestAPIRecoveryOptOutHookPreservesOtherGates(t *testing.T) {
	for _, gate := range []string{"ready", "429", "quota", "terminal"} {
		t.Run(gate, func(t *testing.T) {
			a := newAPIRecoveryTestAccount(1)
			s := &Store{maxConcurrency: 4}
			switch gate {
			case "429":
				s.MarkTransientRateLimited(a, 0)
			case "quota":
				s.MarkCooldown(a, time.Hour, "usage_limit")
			case "terminal":
				s.MarkError(a, "permanent denial")
			}
			status, reason, until, tier := a.Status, a.CooldownReason, a.CooldownUtil, a.HealthTier
			a.mu.Lock()
			a.APIAutoRecoveryEnabled = false
			changed := a.enforceAPIAutoRecoveryOptOutLocked(time.Now())
			a.mu.Unlock()
			if changed || a.Status != status || a.CooldownReason != reason || !a.CooldownUtil.Equal(until) || a.HealthTier != tier {
				t.Fatal("API recovery opt-out modified an unrelated gate")
			}
		})
	}
}

func TestAPIRecoveryCachedExpiryRestoresIndexAndEscalates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a := newAPIRecoveryTestAccount(1)
		other := newAPIRecoveryTestAccount(2)
		s := &Store{accounts: []*Account{a, other}, maxConcurrency: 4, backgroundCtx: ctx}
		s.rebuildAccountIndex()
		s.SetSchedulerEngine("indexed")
		s.applyCachedAccountCooldown(a, runtimeCooldownRecord{
			Kind: cache.CooldownKindTransient, Reason: APIUpstreamUnavailableCooldownReason,
			ResetAt: time.Now().Add(15 * time.Second), UpdatedAt: time.Now(), BackoffLevel: 1,
		})
		if a.IsAvailable() {
			t.Fatal("imported active cooldown allowed dispatch")
		}
		time.Sleep(16 * time.Second)
		synctest.Wait()
		scheduler := s.getFastScheduler()
		scheduler.mu.RLock()
		_, indexed := scheduler.positions[a.DBID]
		scheduler.mu.RUnlock()
		if !indexed || !a.IsAvailable() {
			t.Fatal("imported cooldown expiry failed to restore the maintained index")
		}
		s.ReportRequestSuccess(a, time.Millisecond)
		if got := s.MarkAPIUpstreamUnavailable(a, 0, "rejected again"); got != 30*time.Second {
			t.Fatalf("early success lost shared backoff: next cooldown=%v, want 30s", got)
		}
	})
}
