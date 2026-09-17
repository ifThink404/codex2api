package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteSessionAutoLockSettingsRoundtrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "autolock.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatalf("insert defaults: %v", err)
	}
	s, err := db.GetSystemSettings(ctx)
	if err != nil || s == nil {
		t.Fatalf("GetSystemSettings: %#v, err = %v", s, err)
	}
	if s.CodexSessionAutoLockEnabled || s.CodexSessionAutoLockThreshold != 3 || !s.CodexTurnStateVaultEnabled {
		t.Fatalf("defaults = enabled %v threshold %d vault %v; want false 3 true", s.CodexSessionAutoLockEnabled, s.CodexSessionAutoLockThreshold, s.CodexTurnStateVaultEnabled)
	}
	s.CodexSessionAutoLockEnabled = true
	s.CodexSessionAutoLockThreshold = 7
	s.CodexTurnStateVaultEnabled = false
	if err := db.UpdateSystemSettings(ctx, s); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	s, err = db.GetSystemSettings(ctx)
	if err != nil || s == nil || !s.CodexSessionAutoLockEnabled || s.CodexSessionAutoLockThreshold != 7 || s.CodexTurnStateVaultEnabled {
		t.Fatalf("persisted = %#v, err = %v", s, err)
	}
	s.CodexSessionAutoLockThreshold = 10001
	if err := db.UpdateSystemSettings(ctx, s); err != nil {
		t.Fatalf("UpdateSystemSettings(out of range): %v", err)
	}
	s, err = db.GetSystemSettings(ctx)
	if err != nil || s == nil || s.CodexSessionAutoLockThreshold != 3 {
		t.Fatalf("out-of-range threshold must normalize to 3 on write: %#v, err = %v", s, err)
	}
}

func TestNormalizeSessionAutoLockThreshold(t *testing.T) {
	for in, want := range map[int]int{0: 3, -1: 3, 1: 1, 3: 3, 10000: 10000, 10001: 3} {
		if got := NormalizeSessionAutoLockThreshold(in); got != want {
			t.Errorf("NormalizeSessionAutoLockThreshold(%d) = %d, want %d", in, got, want)
		}
	}
}
