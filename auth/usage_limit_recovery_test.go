package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDispatchUsageLimitRecovery(t *testing.T) {
	now := time.Now()
	short, long := now.Add(time.Hour), now.Add(24*time.Hour)
	for _, tc := range []struct {
		name    string
		change  func(*Account)
		policy  DispatchPolicy
		want    time.Time
		limited bool
	}{
		{"5h", func(a *Account) {}, DispatchPolicyStandard, short, true},
		{"both_windows", func(a *Account) { a.UsagePercent7dValid, a.UsagePercent7d, a.Reset7dAt = true, 100, long }, DispatchPolicyStandard, long, true},
		{"cooldown_longer", func(a *Account) {
			a.Status, a.CooldownReason, a.CooldownUtil = StatusCooldown, ResponsesRateLimitedCooldownReason, long
		}, DispatchPolicyStandard, long, true},
		{"cooldown_only", func(a *Account) {
			a.UsagePercent5hValid = false
			a.Status, a.CooldownReason, a.CooldownUtil = StatusCooldown, ResponsesRateLimitedCooldownReason, short
		}, DispatchPolicyStandard, short, true},
		{"unknown_7d", func(a *Account) { a.UsagePercent7dValid, a.UsagePercent7d = true, 100 }, DispatchPolicyStandard, time.Time{}, true},
		{"stale_free", func(a *Account) {
			a.PlanType = "free"
			a.UsagePercent7dValid, a.UsagePercent7d, a.Reset7dAt = true, 100, now.Add(-time.Hour)
		}, DispatchPolicyStandard, time.Time{}, true},
		{"expired", func(a *Account) { a.Reset5hAt = now.Add(-time.Hour) }, DispatchPolicyStandard, time.Time{}, false},
		{"disabled", func(a *Account) { a.Disabled = 1 }, DispatchPolicyStandard, time.Time{}, false},
		{"credits", func(a *Account) {
			a.CreditEnabled, a.CreditSkipUsageWindow, a.CreditsValid, a.CreditsUnlimited = true, true, true, true
		}, DispatchPolicyStandard, time.Time{}, false},
		{"spark_ignores_standard", func(a *Account) {}, DispatchPolicySpark, time.Time{}, false},
		{"spark_own_reset", func(a *Account) { a.UsagePercentSparkValid, a.UsagePercentSpark, a.ResetSparkAt = true, 100, long }, DispatchPolicySpark, long, true},
		{"spark_unknown", func(a *Account) { a.UsagePercentSparkValid, a.UsagePercentSpark = true, 100 }, DispatchPolicySpark, time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{AccessToken: "test", PlanType: "pro", UsagePercent5hValid: true, UsagePercent5h: 100, Reset5hAt: short}
			tc.change(a)
			got, limited := a.dispatchUsageLimitRecovery(tc.policy, now)
			require.Equal(t, tc.limited, limited)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUsageLimitRecoveryPoolAndFilters(t *testing.T) {
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	short, long := time.Now().Add(time.Hour), time.Now().Add(24*time.Hour)
	store.AddAccount(&Account{DBID: 1, AccessToken: "test-1", PlanType: "pro", UsagePercent5hValid: true, UsagePercent5h: 100, Reset5hAt: short, UsagePercent7dValid: true, UsagePercent7d: 100, Reset7dAt: long})
	store.AddAccount(&Account{DBID: 2, AccessToken: "test-2", PlanType: "pro", UsagePercent5hValid: true, UsagePercent5h: 100, Reset5hAt: short})
	got, limited := store.UsageLimitRecoveryWithDispatch(0, nil, nil, DispatchPolicyStandard)
	require.True(t, limited)
	require.Equal(t, short, got, "pool can recover when the first account recovers all of its windows")
	got, limited = store.UsageLimitRecoveryWithDispatch(0, map[int64]bool{2: true}, nil, DispatchPolicyStandard)
	require.True(t, limited)
	require.Equal(t, long, got)
	got, limited = store.UsageLimitRecoveryWithDispatch(0, nil, func(a *Account) bool { return a.DBID == 1 }, DispatchPolicyStandard)
	require.True(t, limited)
	require.Equal(t, long, got)
	_, limited = store.UsageLimitRecoveryWithDispatch(0, nil, func(a *Account) bool { return false }, DispatchPolicyStandard)
	require.False(t, limited, "unrelated accounts must not generate a usage-limit error")
	store.AddAccount(&Account{DBID: 3, AccessToken: "test-3", PlanType: "free", UsagePercent7dValid: true, UsagePercent7d: 100})
	got, limited = store.UsageLimitRecoveryWithDispatch(0, nil, nil, DispatchPolicyStandard)
	require.True(t, limited)
	require.True(t, got.IsZero(), "unknown resets must not produce a fabricated earliest recovery")
}
