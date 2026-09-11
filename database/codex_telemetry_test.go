package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteCodexTelemetrySettingRoundtrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatalf("insert defaults: %v", err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil || !settings.CodexTelemetryEnabled {
		t.Fatalf("default telemetry setting = %#v, err = %v", settings, err)
	}
	settings.CodexTelemetryEnabled = false
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("disable telemetry: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexTelemetryEnabled {
		t.Fatalf("persisted telemetry setting = %#v, err = %v", settings, err)
	}
}
