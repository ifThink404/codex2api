package auth

import (
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestSessionDispatchFailureUsesSchedulerState(t *testing.T) {
	for _, scenario := range []string{"shared_cooldown", "shared_model_cooldown", "scope", "egress", "dispatch_count", "lazy_credential", "quota_bypass", "bypass_error", "bypass_error_with_cached_quota"} {
		t.Run(scenario, func(t *testing.T) {
			tc := cache.NewMemory(1)
			t.Cleanup(func() { _ = tc.Close() })
			s := NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2})
			a := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "free"}
			s.AddAccount(a)
			policy := DispatchPolicyStandard.WithModel("gpt-5.6-sol")
			want := ""
			switch scenario {
			case "shared_cooldown":
				s.MarkCooldown(a, time.Hour, "server_error")
				a = &Account{DBID: 1, AccessToken: "token", Status: StatusReady}
				want = "account_cooldown"
			case "shared_model_cooldown":
				s.MarkModelCooldown(a, "gpt-5.6-sol", time.Hour, "rate_limited")
				a = &Account{DBID: 1, AccessToken: "token", Status: StatusReady}
				want = "model_cooldown"
			case "scope":
				s.SetAPIKeyAllowedGroups(101, []int64{7})
				want = "api_key_scope_mismatch"
			case "egress":
				a.ProxyURL = "http://disabled.invalid:80"
				s.proxyPoolEnabled = true
				s.managedProxySet = map[string]struct{}{a.ProxyURL: {}}
				s.proxyPoolSet = map[string]struct{}{}
				want = "egress_unavailable"
			case "dispatch_count":
				a.SetDispatchCountLimit(1)
				require.True(t, a.reserveDispatchCount(time.Now()).Allowed)
				want = "account_dispatch_limit"
			case "lazy_credential":
				s.SetLazyMode(true)
				a.AccessToken, a.RefreshToken = "", "refresh"
			case "quota_bypass", "bypass_error", "bypass_error_with_cached_quota":
				a.CodexUsageLimitBypassEnabled, a.CodexUsageLimitBypassModels = true, []string{"gpt-5.6-sol"}
				a.UsagePercent7d, a.UsagePercent7dValid, a.Reset7dAt = 100, true, time.Now().Add(time.Hour)
				if scenario == "bypass_error_with_cached_quota" {
					s.MarkCooldown(a, time.Hour, "usage_limit")
				}
				if scenario == "bypass_error" || scenario == "bypass_error_with_cached_quota" {
					a.Status = StatusError
					want = "account_error"
				}
			}
			require.Equal(t, want, s.SessionDispatchFailure(a, 101, "gpt-5.6-sol", policy))
		})
	}
}

func TestCommittedSessionOwnerClearsOnlyPreviousDenial(t *testing.T) {
	trace := &SelectionTrace{}
	trace.SetSessionModelFilter(func(a *Account) bool { return a.ID() == 2 })
	trace.PinAccount(1)
	require.False(t, trace.CheckSessionModel(&Account{DBID: 1}))
	trace.CommitSessionOwner(2)
	require.EqualValues(t, 2, trace.PinnedAccount())
	require.False(t, trace.SessionModelDenied())
	require.True(t, trace.CheckSessionModel(&Account{DBID: 2}))
	require.False(t, trace.CheckSessionModel(&Account{DBID: 3}), "model constraint must survive the transaction")
}
