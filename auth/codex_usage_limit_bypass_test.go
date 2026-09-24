package auth

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestUsageLimitBypassMatchesExactModelIDsOnly(t *testing.T) {
	a := &Account{CodexUsageLimitBypassEnabled: true, CodexUsageLimitBypassModels: []string{"gpt-6-astra", "gpt-5.6-sol"}}
	for _, model := range []string{"gpt-6-astra", "GPT-5.6-SOL"} {
		require.True(t, a.UsageLimitBypassMatches(DispatchPolicyStandard.WithModel(model)))
	}
	for _, model := range []string{"gpt-6-astra-preview", "gpt-6", "gpt-5.6-luna", "*", ""} {
		require.False(t, a.UsageLimitBypassMatches(DispatchPolicyStandard.WithModel(model)))
	}
	for _, pattern := range []string{"*", "gpt-6-*", "gpt-*-astra"} {
		require.Error(t, ValidateCodexUsageLimitBypassModels([]string{pattern}))
		// Old/imported wildcard values never grant an exemption either.
		a.CodexUsageLimitBypassModels = []string{pattern}
		require.False(t, a.UsageLimitBypassMatches(DispatchPolicyStandard.WithModel("gpt-6-astra")))
		require.False(t, a.hasUsageLimitBypassLocked())
	}
	for _, list := range [][]string{nil, {"gpt-6-astra", "gpt-5.6-sol"}} {
		require.NoError(t, ValidateCodexUsageLimitBypassModels(list))
	}
}

func TestUsageLimitBypassAcrossSchedulers(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed"} {
		for _, lazy := range []bool{false, true} {
			for _, window := range []string{"5h", "7d", "7d_unknown_reset", "both", "auto_pause", "free"} {
				t.Run(fmt.Sprintf("%s/lazy=%t/%s", engine, lazy, window), func(t *testing.T) {
					memory := cache.NewMemory(100)
					t.Cleanup(func() { _ = memory.Close() })
					store := NewStore(nil, memory, &database.SystemSettings{MaxConcurrency: 4, FastSchedulerEnabled: true, SchedulerEngine: engine})
					t.Cleanup(store.Stop)
					store.SetLazyMode(lazy)
					a := &Account{DBID: 1, AccessToken: "test", PlanType: "pro", Status: StatusReady, CodexUsageLimitBypassModels: []string{"gpt-6-astra", "gpt-5.6-sol"}, AutoPause5hThreshold: .8, AutoPause7dThreshold: .8}
					store.AddAccount(a)
					reset := time.Now().Add(time.Hour)
					switch window {
					case "5h":
						a.SetUsageSnapshot5h(100, reset)
						store.MarkPremium5hRateLimited(a, reset)
					case "7d":
						a.SetUsageSnapshot(100, time.Now())
						a.SetReset7dAt(reset)
						store.MarkUsage7dRateLimited(a)
					case "both":
						a.SetUsageSnapshot5h(100, reset)
						a.SetUsageSnapshot(100, time.Now())
						a.SetReset7dAt(reset)
						store.MarkUsage7dRateLimited(a)
					case "7d_unknown_reset":
						a.SetUsageSnapshot(100, time.Now())
						store.MarkUsage7dRateLimited(a)
					case "auto_pause":
						a.SetUsageSnapshot5h(90, reset)
						a.SetUsageSnapshot(90, time.Now())
						a.SetReset7dAt(reset)
					case "free":
						a.PlanType = "free"
						a.SetUsageSnapshot(100, time.Now())
						a.SetReset7dAt(reset)
					}
					// Exemptions are independent of Spark's own exhausted window.
					a.SetUsageSnapshotSpark(100, reset)
					policy := DispatchPolicyStandard.WithModel("gpt-6-astra")
					require.Nil(t, store.NextExcludingWithDispatch(0, nil, nil, policy))
					store.ApplyAccountUsageLimitBypass(1, database.OptionalBool{Set: true, Value: true}, database.OptionalStringSlice{})
					for _, p := range []DispatchPolicy{DispatchPolicyStandard, DispatchPolicyStandard.WithModel("gpt-6-sol"), DispatchPolicySpark} {
						require.Nil(t, store.NextExcludingWithDispatch(0, nil, nil, p))
					}
					take := func(got *Account) { require.Same(t, a, got); store.Release(got) }
					take(store.NextExcludingWithDispatch(0, nil, nil, policy))
					take(store.NextExcludingWithDispatch(0, nil, nil, DispatchPolicyStandard.WithModel("gpt-5.6-sol")))
					take(store.TakePreferredAccountWithDispatch(1, 0, nil, nil, policy))
					got, _, _ := store.NextForSessionWithDispatchGuard("test-root", 0, nil, nil, policy)
					take(got)
					store.BindSessionAffinity("test-root", a, "")
					got, _ = store.NextForContinuationWithDispatch("test-root", 0, nil, nil, policy)
					take(got)
					_, limited := store.UsageLimitRecoveryWithDispatch(0, nil, nil, policy)
					require.False(t, limited)
					require.False(t, a.IsAvailable(), "do not erase actual account quota status")
					require.Empty(t, store.CollectCleanTargets("rate_limited", nil))
					require.Empty(t, store.CollectCleanTargets("usage_exhausted", nil))
					require.Zero(t, store.CleanFullUsageAccounts(t.Context()))
					require.Same(t, a, store.FindByID(a.ID()))
					store.ApplyAccountUsageLimitBypass(1, database.OptionalBool{Set: true}, database.OptionalStringSlice{})
					require.Nil(t, store.NextExcludingWithDispatch(0, nil, nil, policy))
				})
			}
		}
	}
}

func TestUsageLimitBypassRetainsHardGates(t *testing.T) {
	for _, scenario := range []string{"error", "banned", "disabled", "paused", "no_credential", "real_429", "unauthorized", "overload", "concurrency", "filter", "excluded", "key_scope", "relay", "empty_models", "spark"} {
		t.Run(scenario, func(t *testing.T) {
			store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			t.Cleanup(store.Stop)
			a := &Account{DBID: 1, AccessToken: "test", PlanType: "pro", Status: StatusReady, CodexUsageLimitBypassEnabled: true, CodexUsageLimitBypassModels: []string{"gpt-6-astra"}}
			store.AddAccount(a)
			a.SetUsageSnapshot5h(100, time.Now().Add(time.Hour))
			policy := DispatchPolicyStandard.WithModel("gpt-6-astra")
			var filter AccountFilter
			var exclude map[int64]bool
			switch scenario {
			case "error":
				a.Status = StatusError
			case "banned":
				a.HealthTier = HealthTierBanned
			case "disabled":
				atomic.StoreInt32(&a.Disabled, 1)
			case "paused":
				atomic.StoreInt32(&a.DispatchPaused, 1)
			case "no_credential":
				a.AccessToken = ""
			case "real_429", "unauthorized", "overload":
				a.Status, a.CooldownUtil = StatusCooldown, time.Now().Add(time.Minute)
				a.CooldownReason = map[string]string{"real_429": "rate_limited", "unauthorized": "unauthorized", "overload": "server_is_overloaded"}[scenario]
			case "concurrency":
				atomic.StoreInt64(&a.OccupiedRequests, 100)
			case "filter":
				filter = func(*Account) bool { return false }
			case "excluded":
				exclude = map[int64]bool{1: true}
			case "key_scope":
				a.AllowedAPIKeyIDs = []int64{999}
			case "relay":
				a.UpstreamType = UpstreamOpenAIResponses
				require.False(t, a.UsageLimitBypassMatches(policy))
				return // Relays use their own quota policy.
			case "empty_models":
				a.CodexUsageLimitBypassModels = nil
			case "spark":
				policy = DispatchPolicySpark
				a.SetUsageSnapshotSpark(100, time.Now().Add(time.Hour))
			}
			require.Nil(t, store.NextExcludingWithDispatch(1, exclude, filter, policy))
			require.Nil(t, store.TakePreferredAccountWithDispatch(1, 1, exclude, filter, policy))
			if scenario == "real_429" {
				recovery, limited := store.UsageLimitRecoveryWithDispatch(1, nil, nil, policy)
				require.True(t, limited)
				require.Equal(t, a.CooldownUtil, recovery)
			}
		})
	}
}
