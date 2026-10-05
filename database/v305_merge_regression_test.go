package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestV305SettingsKeepBasispointsAndSessionGuardsAligned guards the v3.0.5
// merge, where both sides appended settings columns: every upstream
// Basispoints field and every local session-guard field must round-trip
// together, and the two preserve-flag parameters must stay after the values.
func TestV305SettingsKeepBasispointsAndSessionGuardsAligned(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "merged-settings.db")
			if driver == "postgres" {
				dsn = os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
				}
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()

			// Alternate neighbouring booleans and use non-default numbers so a
			// shifted placeholder lands on a visibly wrong value.
			s := &SystemSettings{
				CodexTurnStateStrict: true, CodexSessionNoBorrowEnabled: false,
				CodexSessionNoBorrowHoldSeconds: 25, CodexInitialSessionAdmissionEnabled: true,
				CodexInitialSessionMaxAgeSeconds: 600, CodexSessionAutoLockEnabled: false,
				CodexSessionAutoLockThreshold: 7, CodexTurnStateVaultEnabled: false,
				AutoResetCreditsOnExhaustionEnabled: true,
				CodexUnifiedClientIdentityEnabled:   false,
				ShowUpstreamModelMismatch:           true,
				PromptFilterCustomPatterns:          `[{"id":"keep"}]`,
				PromptFilterReviewAPIKey:            "test-review-key",
			}
			check := func(stage string, want *SystemSettings, wantPatterns, wantKey string) {
				t.Helper()
				got, err := db.GetSystemSettings(ctx)
				if err != nil || got == nil {
					t.Fatalf("%s: GetSystemSettings: settings=%v err=%v", stage, got != nil, err)
				}
				if got.CodexTurnStateStrict != want.CodexTurnStateStrict ||
					got.CodexSessionNoBorrowEnabled != want.CodexSessionNoBorrowEnabled ||
					got.CodexSessionNoBorrowHoldSeconds != want.CodexSessionNoBorrowHoldSeconds ||
					got.CodexInitialSessionAdmissionEnabled != want.CodexInitialSessionAdmissionEnabled ||
					got.CodexInitialSessionMaxAgeSeconds != want.CodexInitialSessionMaxAgeSeconds ||
					got.CodexSessionAutoLockEnabled != want.CodexSessionAutoLockEnabled ||
					got.CodexSessionAutoLockThreshold != want.CodexSessionAutoLockThreshold ||
					got.CodexTurnStateVaultEnabled != want.CodexTurnStateVaultEnabled ||
					got.AutoResetCreditsOnExhaustionEnabled != want.AutoResetCreditsOnExhaustionEnabled {
					t.Fatalf("%s: session guard fields shifted", stage)
				}
				if got.CodexUnifiedClientIdentityEnabled != want.CodexUnifiedClientIdentityEnabled ||
					got.ShowUpstreamModelMismatch != want.ShowUpstreamModelMismatch {
					t.Fatalf("%s: v3.0.6 fields shifted", stage)
				}
				if got.PromptFilterCustomPatterns != wantPatterns || got.PromptFilterReviewAPIKey != wantKey {
					t.Fatalf("%s: preserve flags shifted: patterns=%q key=%q", stage, got.PromptFilterCustomPatterns, got.PromptFilterReviewAPIKey)
				}
			}

			if err := db.UpdateSystemSettings(ctx, s); err != nil {
				t.Fatal(err)
			}
			check("first save", s, `[{"id":"keep"}]`, "test-review-key")

			// Flip every field, and ask the preserve flags to keep the stored
			// prompt rules and review key despite the stale values sent here.
			s.CodexTurnStateStrict, s.CodexSessionNoBorrowEnabled = false, true
			s.CodexSessionNoBorrowHoldSeconds, s.CodexInitialSessionAdmissionEnabled = 11, false
			s.CodexInitialSessionMaxAgeSeconds, s.CodexSessionAutoLockEnabled = 900, true
			s.CodexSessionAutoLockThreshold, s.CodexTurnStateVaultEnabled = 9, true
			s.AutoResetCreditsOnExhaustionEnabled = false
			s.CodexUnifiedClientIdentityEnabled, s.ShowUpstreamModelMismatch = true, false
			s.PreservePromptFilterCustomPatterns, s.PreservePromptFilterReviewAPIKey = true, true
			s.PromptFilterCustomPatterns, s.PromptFilterReviewAPIKey = "[]", ""
			if err := db.UpdateSystemSettings(ctx, s); err != nil {
				t.Fatal(err)
			}
			check("flipped save", s, `[{"id":"keep"}]`, "test-review-key")
		})
	}
}
