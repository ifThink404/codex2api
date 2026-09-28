package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func proxyLocationTestDBs(t *testing.T) map[string]func() *DB {
	t.Helper()
	dir := t.TempDir()
	open := map[string]func() *DB{
		"sqlite": func() *DB {
			db, err := New("sqlite", filepath.Join(dir, "codex2api.db"))
			if err != nil {
				t.Fatalf("New(sqlite): %v", err)
			}
			return db
		},
	}
	if dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN"); dsn != "" {
		open["postgres"] = func() *DB {
			db, err := New("postgres", dsn)
			if err != nil {
				t.Fatalf("New(postgres): %v", err)
			}
			return db
		}
	}
	return open
}

func TestProxyTestGeoRoundTripAndClearing(t *testing.T) {
	for dialect, open := range proxyLocationTestDBs(t) {
		t.Run(dialect, func(t *testing.T) {
			db := open()
			defer db.Close()
			ctx := context.Background()
			const proxyURL = "http://geo.example:8080"
			if _, err := db.conn.ExecContext(ctx, `DELETE FROM proxies WHERE url IN ($1, $2)`, proxyURL, "http://geo-2.example:8080"); err != nil {
				t.Fatalf("reset: %v", err)
			}
			id, err := db.InsertProxy(ctx, proxyURL, "")
			if err != nil {
				t.Fatalf("InsertProxy: %v", err)
			}
			t.Cleanup(func() { _, _ = db.conn.ExecContext(context.Background(), `DELETE FROM proxies WHERE id = $1`, id) })
			geo := ProxyTestGeo{CountryCode: "us", Region: "California", City: "Los Angeles"}
			if err := db.UpdateProxyTestResultWithGeo(ctx, id, proxyURL, ProxyTestStatusSuccess, "1.2.3.4", "US·CA·LA", "America/Los_Angeles", 50, geo); err != nil {
				t.Fatalf("UpdateProxyTestResultWithGeo: %v", err)
			}
			row, err := db.GetProxy(ctx, id)
			if err != nil {
				t.Fatalf("GetProxy: %v", err)
			}
			want := ProxyLocation{Country: "US", Region: "California", City: "Los Angeles", Timezone: "America/Los_Angeles"}
			if got := row.Location(); got != want {
				t.Fatalf("Location() = %+v, want %+v", got, want)
			}
			listed, err := db.ListEnabledProxies(ctx)
			if err != nil {
				t.Fatalf("ListEnabledProxies: %v", err)
			}
			found := false
			for _, item := range listed {
				if item.ID == id {
					found = item.Location() == want
				}
			}
			if !found {
				t.Fatal("ListEnabledProxies did not return the stored location")
			}

			// Editing the URL invalidates the detected location.
			newURL := "http://geo-2.example:8080"
			if err := db.UpdateProxy(ctx, id, &newURL, nil, nil); err != nil {
				t.Fatalf("UpdateProxy: %v", err)
			}
			if row, _ = db.GetProxy(ctx, id); row.Location() != (ProxyLocation{}) {
				t.Fatalf("location after URL change = %+v, want empty", row.Location())
			}

			if err := db.UpdateProxyTestResultWithGeo(ctx, id, newURL, ProxyTestStatusSuccess, "1.2.3.4", "x", "", 1, geo); err != nil {
				t.Fatalf("second success: %v", err)
			}
			if err := db.UpdateProxyTestResult(ctx, id, newURL, ProxyTestStatusError, "", "", "", 0); err != nil {
				t.Fatalf("failed test: %v", err)
			}
			if row, _ = db.GetProxy(ctx, id); row.Location() != (ProxyLocation{}) {
				t.Fatalf("location after failed test = %+v, want empty", row.Location())
			}
		})
	}
}

func TestCodexWebSearchProxyLocationPersists(t *testing.T) {
	for dialect, open := range proxyLocationTestDBs(t) {
		t.Run(dialect, func(t *testing.T) {
			db := open()
			if db.GetCodexWebSearchProxyLocation() && dialect == "sqlite" {
				t.Fatal("switch must default to off")
			}
			if err := db.SaveCodexWebSearchProxyLocation(context.Background(), true); err != nil {
				t.Fatalf("Save(true): %v", err)
			}
			_ = db.Close()
			reopened := open()
			if !reopened.GetCodexWebSearchProxyLocation() {
				t.Fatal("switch did not survive a restart")
			}
			if err := reopened.SaveCodexWebSearchProxyLocation(context.Background(), false); err != nil {
				t.Fatalf("Save(false): %v", err)
			}
			if reopened.GetCodexWebSearchProxyLocation() {
				t.Fatal("in-process switch not updated")
			}
			_ = reopened.Close()
		})
	}
}

func TestNormalizeProxyLocationFields(t *testing.T) {
	if NormalizeProxyCountryCode(" jp ") != "JP" || NormalizeProxyCountryCode("USA") != "" || NormalizeProxyCountryCode("1A") != "" {
		t.Fatal("country code normalization")
	}
	if NormalizeProxyLocationText("New\nYork") != "" || NormalizeProxyLocationText(" Osaka ") != "Osaka" {
		t.Fatal("location text normalization")
	}
}
