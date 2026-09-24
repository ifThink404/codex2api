package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexForkAccountFallbackSettingsSQLiteDefaultsAndMigration(test *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(test.TempDir(), "fork-fallback.db")
	db, err := New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	if _, err := db.conn.ExecContext(ctx, "INSERT INTO system_settings (id) VALUES (1)"); err != nil {
		test.Fatal(err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexForkAccountFallbackEnabled {
		test.Fatalf("fresh setting must default off: settings=%+v err=%v", settings, err)
	}
	if _, err := db.conn.ExecContext(ctx, "UPDATE system_settings SET codex_fork_account_fallback_enabled = NULL WHERE id = 1"); err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexForkAccountFallbackEnabled {
		test.Fatalf("null setting must default off: settings=%+v err=%v", settings, err)
	}
	settings.SiteName = "existing installation"
	settings.CodexImagesMainModel = "gpt-5.6-luna"
	settings.CodexTelemetryEnabled = true
	settings.CodexForkAccountFallbackEnabled = true
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		test.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN codex_fork_account_fallback_enabled"); err != nil {
		test.Fatal(err)
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		test.Fatalf("read migrated settings: %v", err)
	}
	if settings.CodexForkAccountFallbackEnabled || settings.SiteName != "existing installation" || settings.CodexImagesMainModel != "gpt-5.6-luna" || !settings.CodexTelemetryEnabled {
		test.Fatal("migration enabled failover or changed existing settings")
	}
	testCodexForkAccountFallbackSettingsRoundTrip(test, db)
}

func TestCodexForkAccountFallbackSettingsPostgres(test *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		test.Skip("requires an isolated CODEX2API_TEST_POSTGRES_DSN database")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	testCodexForkAccountFallbackSettingsRoundTrip(test, db)
}

func testCodexForkAccountFallbackSettingsRoundTrip(test *testing.T, db *DB) {
	test.Helper()
	ctx := context.Background()
	for _, enabled := range []bool{true, false} {
		for _, preservePatterns := range []bool{true, false} {
			for _, preserveKey := range []bool{true, false} {
				test.Run(fmt.Sprintf("enabled=%t/patterns=%t/key=%t", enabled, preservePatterns, preserveKey), func(test *testing.T) {
					settings := &SystemSettings{
						CodexForkAccountFallbackEnabled: !enabled,
						CodexImagesMainModel:            "gpt-5.6-luna",
						CodexTelemetryEnabled:           true,
						PromptFilterCustomPatterns:      "[]",
						PromptFilterReviewAPIKey:        "original-key",
					}
					if err := db.UpdateSystemSettings(ctx, settings); err != nil {
						test.Fatal(err)
					}
					settings.CodexForkAccountFallbackEnabled = enabled
					settings.PromptFilterCustomPatterns = `[{"id":"new","pattern":"new"}]`
					settings.PromptFilterReviewAPIKey = "new-key"
					settings.PreservePromptFilterCustomPatterns = preservePatterns
					settings.PreservePromptFilterReviewAPIKey = preserveKey
					if err := db.UpdateSystemSettings(ctx, settings); err != nil {
						test.Fatal(err)
					}
					persisted, err := db.GetSystemSettings(ctx)
					if err != nil || persisted == nil {
						test.Fatalf("read updated settings: %v", err)
					}
					if persisted.CodexForkAccountFallbackEnabled != enabled || persisted.CodexImagesMainModel != settings.CodexImagesMainModel || !persisted.CodexTelemetryEnabled {
						test.Fatal("failover did not round trip independently of adjacent SQL parameters")
					}
					wantPatterns, wantKey := settings.PromptFilterCustomPatterns, settings.PromptFilterReviewAPIKey
					if preservePatterns {
						wantPatterns = "[]"
					}
					if preserveKey {
						wantKey = "original-key"
					}
					if persisted.PromptFilterCustomPatterns != wantPatterns || persisted.PromptFilterReviewAPIKey != wantKey {
						test.Fatal("prompt filter preservation guards no longer match their SQL parameters")
					}
				})
			}
		}
	}
}
