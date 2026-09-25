package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSessionBalanceSettingsMigrationAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "balance.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []bool{false, true} {
		if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET session_balance_mode = '', session_window_balance_enabled = $1 WHERE id = 1`, legacy); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetSystemSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := SessionBalanceDefault
		if legacy {
			want = SessionBalanceSession
		}
		if got.SessionBalanceMode != want {
			t.Fatalf("legacy %t became %q, want %q", legacy, got.SessionBalanceMode, want)
		}
	}
	for _, mode := range []string{SessionBalanceWindow, SessionBalanceSession, SessionBalanceDefault} {
		settings := &SystemSettings{MaxConcurrency: 8, TestConcurrency: 1, TestModel: "test", SessionBalanceMode: mode, SessionWindowBalanceEnabled: true}
		if err := db.UpdateSystemSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = New("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.GetSystemSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.SessionBalanceMode != mode || got.SessionWindowBalanceEnabled != (mode != SessionBalanceDefault) {
			t.Fatalf("restart changed mode %q to %q / %t", mode, got.SessionBalanceMode, got.SessionWindowBalanceEnabled)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
