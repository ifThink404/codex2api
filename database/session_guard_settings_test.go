package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteSessionGuardSettingsRoundtrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "guards.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatalf("insert defaults: %v", err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		t.Fatalf("GetSystemSettings: %#v, err = %v", settings, err)
	}
	if settings.CodexTurnStateStrict || settings.CodexSessionNoBorrowEnabled || settings.CodexInitialSessionAdmissionEnabled {
		t.Fatalf("guard switches must default off: %#v", settings)
	}
	if settings.CodexSessionNoBorrowHoldSeconds != 20 || settings.CodexInitialSessionMaxAgeSeconds != 180 {
		t.Fatalf("guard defaults = hold %d, max age %d; want 20, 180", settings.CodexSessionNoBorrowHoldSeconds, settings.CodexInitialSessionMaxAgeSeconds)
	}

	settings.CodexTurnStateStrict = true
	settings.CodexSessionNoBorrowEnabled = true
	settings.CodexSessionNoBorrowHoldSeconds = 25
	settings.CodexInitialSessionAdmissionEnabled = true
	settings.CodexInitialSessionMaxAgeSeconds = 600
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		t.Fatalf("reload: %#v, err = %v", settings, err)
	}
	if !settings.CodexTurnStateStrict || !settings.CodexSessionNoBorrowEnabled || !settings.CodexInitialSessionAdmissionEnabled {
		t.Fatalf("guard switches did not persist: %#v", settings)
	}
	if settings.CodexSessionNoBorrowHoldSeconds != 25 || settings.CodexInitialSessionMaxAgeSeconds != 600 {
		t.Fatalf("guard numbers did not persist: hold %d, max age %d", settings.CodexSessionNoBorrowHoldSeconds, settings.CodexInitialSessionMaxAgeSeconds)
	}

	settings.CodexSessionNoBorrowHoldSeconds = 999
	settings.CodexInitialSessionMaxAgeSeconds = 0
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings(out of range): %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		t.Fatalf("reload: %#v, err = %v", settings, err)
	}
	if settings.CodexSessionNoBorrowHoldSeconds != 30 || settings.CodexInitialSessionMaxAgeSeconds != 180 {
		t.Fatalf("out-of-range values must normalize on write: hold %d, max age %d", settings.CodexSessionNoBorrowHoldSeconds, settings.CodexInitialSessionMaxAgeSeconds)
	}
}

func TestNormalizeSessionGuardNumbers(t *testing.T) {
	cases := []struct{ in, hold, age int }{
		{0, 20, 180}, {-5, 20, 180}, {1, 1, 1}, {30, 30, 30}, {31, 30, 31}, {86400, 30, 86400}, {86401, 30, 180},
	}
	for _, tc := range cases {
		if got := NormalizeSessionNoBorrowHoldSeconds(tc.in); got != tc.hold {
			t.Errorf("NormalizeSessionNoBorrowHoldSeconds(%d) = %d, want %d", tc.in, got, tc.hold)
		}
		if got := NormalizeCodexInitialSessionMaxAgeSeconds(tc.in); got != tc.age {
			t.Errorf("NormalizeCodexInitialSessionMaxAgeSeconds(%d) = %d, want %d", tc.in, got, tc.age)
		}
	}
}
