package auth

import "testing"

func TestAPIAutoRecoveryRequiresExplicitKeyOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, key := range []bool{false, true} {
			a := &Account{UpstreamType: UpstreamGrok, AccessToken: "oauth", APIAutoRecoveryEnabled: enabled}
			if key {
				a.APIKey = "key"
			}
			if got := a.APIAutoRecoveryEnabledForAccount(); got != (key && enabled) {
				t.Fatalf("key=%v enabled=%v got=%v", key, enabled, got)
			}
			a.mu.RLock()
			got := a.apiAutoRecoveryEnabledLocked()
			a.mu.RUnlock()
			if got != (key && enabled) {
				t.Fatal("locked predicate disagrees")
			}
		}
	}
	if (*Account)(nil).APIAutoRecoveryEnabledForAccount() {
		t.Fatal("nil opted in")
	}
}
