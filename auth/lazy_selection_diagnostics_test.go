package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLazySelectionRecordsConcreteRejectionWithoutChangingAvailability(t *testing.T) {
	for _, scenario := range []struct {
		name, reason string
		configure    func(*Account)
	}{
		{"paused", "account_paused", func(a *Account) { a.DispatchPaused = 1 }},
		{"disabled", "account_disabled", func(a *Account) { a.Disabled = 1 }},
		{"error", "account_error", func(a *Account) { a.Status = StatusError }},
		{"banned", "account_banned", func(a *Account) { a.HealthTier = HealthTierBanned }},
		{"credentials", "credential_unavailable", func(a *Account) { a.AccessToken = "" }},
		{"cooldown", "account_cooldown", func(a *Account) {
			a.Status = StatusCooldown
			a.CooldownUtil = time.Now().Add(time.Hour)
			a.CooldownReason = "unauthorized"
		}},
		{"7d", "account_usage_exhausted", func(a *Account) {
			a.UsagePercent7d = 100
			a.UsagePercent7dValid = true
			a.Reset7dAt = time.Now().Add(time.Hour)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, account := newSessionCapacityTestStore(1)
			store.SetLazyMode(true)
			scenario.configure(account)
			trace := &SelectionTrace{}
			trace.EnableCandidateDetails()
			trace.Reset()
			require.Nil(t, store.TakePreferredAccountWithDispatch(account.ID(), 0, nil, nil, DispatchPolicyStandard, trace))
			require.Contains(t, trace.Snapshot().Reasons, "lazy_account_unavailable")
			require.Contains(t, trace.Snapshot().Reasons, scenario.reason)
			details := trace.CandidateDetails()
			require.NotEmpty(t, details.Samples)
			require.Equal(t, account.ID(), details.Samples[0].AccountID)
			require.Equal(t, scenario.reason, details.Samples[0].Reason)
			if scenario.name == "7d" {
				require.True(t, details.Samples[0].State.Usage7dBlocked)
			}
			if scenario.name == "cooldown" {
				require.Equal(t, "unauthorized", details.Samples[0].State.CooldownReason)
			}
		})
	}
}
