package auth

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestProbePolicyPatchCancelsPendingRecoveryEvenAlreadyFalse(t *testing.T) {
	for _, alreadyFalse := range []bool{false, true} {
		s := NewStore(nil, nil, nil)
		a := &Account{DBID: 1, UpstreamType: UpstreamOpenAIResponses, APIKey: "key", BaseURL: "https://example.invalid", Status: StatusReady, APIAutoRecoveryEnabled: true}
		s.AddAccount(a)
		s.MarkAPIUpstreamUnavailable(a, time.Minute, "local auth failure")
		atomic.StoreInt32(&a.DispatchPaused, 1)
		a.mu.Lock()
		if alreadyFalse {
			a.APIAutoRecoveryEnabled = false
		}
		a.mu.Unlock()
		s.ApplyAccountProbePolicyPatch(a.DBID, map[string]interface{}{APIAutoRecoveryCredentialKey: false})
		if a.GetCooldownReason() != "unauthorized" || !a.IsBanned() || a.IsAvailable() || a.ErrorMsg != "local auth failure" || atomic.LoadInt32(&a.DispatchPaused) != 1 {
			t.Fatalf("opt-out did not preserve legacy auth gate: reason=%s banned=%v", a.GetCooldownReason(), a.IsBanned())
		}
		a.mu.RLock()
		pending := a.transientRateLimitTimer != nil || !a.transientRateLimitUntil.IsZero()
		a.mu.RUnlock()
		if pending {
			t.Fatal("opt-out left recovery armed")
		}
		s.Stop()
	}
}

func TestProbePolicyOutboxPreservesLocalPendingRecovery(t *testing.T) {
	for _, tc := range []struct {
		name             string
		pending, enabled bool
		status           AccountStatus
		reason           string
	}{
		{"opt-out", true, false, StatusReady, "unauthorized"},
		{"still-enabled", true, true, StatusReady, APIUpstreamUnavailableCooldownReason},
		{"no-local-pending", false, false, StatusReady, ""},
		{"stronger-terminal", true, false, StatusError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(nil, nil, nil)
			defer s.Stop()
			a := &Account{DBID: 1, UpstreamType: UpstreamOpenAIResponses, APIKey: "key", BaseURL: "https://example.invalid", Status: StatusReady, APIAutoRecoveryEnabled: true}
			s.AddAccount(a)
			if tc.pending {
				s.MarkAPIUpstreamUnavailable(a, time.Minute, "local auth failure")
			}
			src := &Account{DBID: 1, UpstreamType: UpstreamOpenAIResponses, APIKey: "key", BaseURL: a.BaseURL, Status: tc.status, APIAutoRecoveryEnabled: tc.enabled}
			s.applyPersistentAccountSnapshot(a, src, true)
			if a.GetCooldownReason() != tc.reason {
				t.Fatalf("outbox lost local gate: got=%s want=%s", a.GetCooldownReason(), tc.reason)
			}
			if tc.pending && tc.status == StatusReady && (a.IsAvailable() || a.ErrorMsg != "local auth failure") {
				t.Fatal("snapshot overwrote pending failure")
			}
			if !tc.pending && a.IsBanned() {
				t.Fatal("snapshot manufactured local auth failure")
			}
			if tc.status == StatusError && a.Status != StatusError {
				t.Fatal("snapshot terminal gate overwritten")
			}
		})
	}
}

func TestProbePolicyOutboxNewCredentialDropsOldRecovery(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s := NewStore(nil, nil, nil)
		a := &Account{DBID: 1, CredentialGeneration: 1, UpstreamType: UpstreamOpenAIResponses, APIKey: "old", BaseURL: "https://example.invalid", Status: StatusReady, APIAutoRecoveryEnabled: true}
		s.AddAccount(a)
		s.MarkAPIUpstreamUnavailable(a, time.Minute, "old credential rejected")
		src := &Account{DBID: 1, CredentialGeneration: 2, UpstreamType: UpstreamOpenAIResponses, APIKey: "new", BaseURL: a.BaseURL, Status: StatusReady, APIAutoRecoveryEnabled: enabled, HealthTier: HealthTierHealthy}
		s.applyPersistentAccountSnapshot(a, src, true)
		if a.GetCooldownReason() != "" || a.IsBanned() || a.ErrorMsg != "" || !a.IsAvailable() {
			t.Fatalf("new credential inherited old failure: reason=%s banned=%v status=%v", a.GetCooldownReason(), a.IsBanned(), a.Status)
		}
		a.mu.RLock()
		pending := a.transientRateLimitTimer != nil || !a.transientRateLimitUntil.IsZero() || a.transientRateLimitBackoff != 0
		a.mu.RUnlock()
		if pending {
			t.Fatal("old credential recovery still armed")
		}
		s.Stop()
	}
}
