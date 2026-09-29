package auth

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountCredentialInvalid(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		acc  *Account
		want bool
	}{
		{name: "ready", acc: &Account{Status: StatusReady}, want: false},
		{name: "rate limit cooldown", acc: &Account{Status: StatusCooldown, CooldownReason: "rate_limited", CooldownUtil: now.Add(time.Hour)}, want: false},
		{name: "401 cooldown", acc: &Account{Status: StatusCooldown, CooldownReason: "unauthorized", CooldownUtil: now.Add(5 * time.Minute)}, want: true},
		{name: "expired 401 cooldown", acc: &Account{Status: StatusCooldown, CooldownReason: "unauthorized", CooldownUtil: now.Add(-time.Minute)}, want: false},
		{name: "error status", acc: &Account{Status: StatusError}, want: true},
		{name: "banned", acc: &Account{Status: StatusReady, HealthTier: HealthTierBanned}, want: true},
		{name: "nil", acc: nil, want: true},
	} {
		if got := tc.acc.CredentialInvalid(); got != tc.want {
			t.Errorf("%s: CredentialInvalid() = %v, want %v", tc.name, got, tc.want)
		}
	}
	flagged := &Account{Status: StatusReady}
	atomic.StoreInt32(&flagged.Disabled, 1)
	if !flagged.CredentialInvalid() {
		t.Error("the 401 Disabled flag marks the credential invalid")
	}
	paused := &Account{Status: StatusReady}
	atomic.StoreInt32(&paused.DispatchPaused, 1)
	if paused.CredentialInvalid() || paused.IsEnabled() {
		t.Error("an admin-disabled account is not enabled but its credential is fine")
	}
}
