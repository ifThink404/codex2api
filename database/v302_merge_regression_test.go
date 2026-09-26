package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestV302SettingsKeepSessionGuardsAndClientBuilds(t *testing.T) {
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
			s := &SystemSettings{
				CodexTurnStateStrict: true, CodexSessionNoBorrowEnabled: true,
				CodexSessionNoBorrowHoldSeconds: 25, CodexInitialSessionAdmissionEnabled: true,
				CodexInitialSessionMaxAgeSeconds: 600, CodexSessionAutoLockEnabled: true,
				CodexSessionAutoLockThreshold: 5, CodexTurnStateVaultEnabled: false,
				AutoResetCreditsOnExhaustionEnabled: true,
				PromptFilterCustomPatterns:          `[{"id":"keep"}]`, PromptFilterReviewAPIKey: "test-review-key",
			}
			if err := db.UpdateSystemSettings(ctx, s); err != nil {
				t.Fatal(err)
			}
			for kind, build := range map[string]string{"desktop-mac": "101", "desktop-windows": "202", "vscode": "303"} {
				if err := db.UpdateCodexSyncedAppBuild(ctx, kind, build); err != nil {
					t.Fatal(err)
				}
			}
			// A stale full-settings save must preserve independently synced builds
			// and keep the two preserve-flag SQL parameters in the right positions.
			s.PreservePromptFilterCustomPatterns = true
			s.PreservePromptFilterReviewAPIKey = true
			s.PromptFilterCustomPatterns = "[]"
			s.PromptFilterReviewAPIKey = ""
			if err := db.UpdateSystemSettings(ctx, s); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetSystemSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("missing settings")
			}
			if !got.CodexTurnStateStrict || !got.CodexSessionNoBorrowEnabled || got.CodexSessionNoBorrowHoldSeconds != 25 ||
				!got.CodexInitialSessionAdmissionEnabled || got.CodexInitialSessionMaxAgeSeconds != 600 ||
				!got.CodexSessionAutoLockEnabled || got.CodexSessionAutoLockThreshold != 5 || got.CodexTurnStateVaultEnabled {
				t.Fatal("local session guards did not survive settings round trip")
			}
			if !got.AutoResetCreditsOnExhaustionEnabled || got.AutoResetCreditsEnabled {
				t.Fatal("reset switches mixed")
			}
			if got.CodexSyncedDesktopMacBuild != "101" || got.CodexSyncedDesktopWindowsBuild != "202" || got.CodexSyncedVSCodeBuild != "303" {
				t.Fatal("independently synced builds were lost")
			}
			if got.PromptFilterCustomPatterns != `[{"id":"keep"}]` || got.PromptFilterReviewAPIKey != "test-review-key" {
				t.Fatal("preserve flags shifted during merge")
			}
		})
	}
}
