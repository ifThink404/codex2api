package database

import (
	"context"
	"os"
	"testing"
)

// TestUpdateProxyTestResultRoundTripsTimezone 覆盖 test_timezone 列的写入与回读，
// 以及探测失败时该列会像 test_location 一样被清空。
func TestUpdateProxyTestResultRoundTripsTimezone(t *testing.T) {
	db := newProxyTestDB(t)
	ctx := context.Background()

	id, err := db.InsertProxy(ctx, "http://tz.example:8080", "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}

	if got := findProxyRow(t, db, id).TestTimezone; got != "" {
		t.Fatalf("new proxy test_timezone = %q, want empty", got)
	}

	if err := db.UpdateProxyTestResult(ctx, id, "http://tz.example:8080", ProxyTestStatusSuccess, "1.2.3.4", "US·CA·LA", "America/Los_Angeles", 123); err != nil {
		t.Fatalf("UpdateProxyTestResult(success) returned error: %v", err)
	}
	if got := findProxyRow(t, db, id).TestTimezone; got != "America/Los_Angeles" {
		t.Fatalf("test_timezone after success = %q, want %q", got, "America/Los_Angeles")
	}

	if err := db.UpdateProxyTestResult(ctx, id, "http://tz.example:8080", ProxyTestStatusError, "", "", "", 0); err != nil {
		t.Fatalf("UpdateProxyTestResult(error) returned error: %v", err)
	}
	if got := findProxyRow(t, db, id).TestTimezone; got != "" {
		t.Fatalf("test_timezone after failed probe = %q, want empty (cleared like test_location)", got)
	}
}

// TestPostgresProxyTimezoneRoundTrip 覆盖 PostgreSQL 上 test_timezone 列的
// ADD COLUMN 迁移、SELECT 列表与 UpdateProxyTestResult 的写入/清空。
//
//	docker run -d --rm --name c2a-pg-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=codex2api_test -p 55432:5432 postgres:16
//	CODEX2API_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/codex2api_test?sslmode=disable' \
//	    go test ./database/ -run TestPostgresProxyTimezoneRoundTrip -count=1
func TestPostgresProxyTimezoneRoundTrip(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.conn.ExecContext(ctx, `DELETE FROM proxies WHERE url = $1`, "http://pg-tz.example:8080"); err != nil {
		t.Fatalf("reset proxy row: %v", err)
	}

	id, err := db.InsertProxy(ctx, "http://pg-tz.example:8080", "pg-tz")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.conn.ExecContext(context.Background(), `DELETE FROM proxies WHERE id = $1`, id)
	})

	row, err := db.GetProxy(ctx, id)
	if err != nil {
		t.Fatalf("GetProxy returned error: %v", err)
	}
	if row.TestTimezone != "" {
		t.Fatalf("new proxy test_timezone = %q, want empty", row.TestTimezone)
	}

	if err := db.UpdateProxyTestResult(ctx, id, "http://pg-tz.example:8080", ProxyTestStatusSuccess, "1.2.3.4", "US·CA·LA", "America/Los_Angeles", 123); err != nil {
		t.Fatalf("UpdateProxyTestResult(success) returned error: %v", err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil {
		t.Fatalf("GetProxy returned error: %v", err)
	}
	if row.TestTimezone != "America/Los_Angeles" {
		t.Fatalf("test_timezone after success = %q, want %q", row.TestTimezone, "America/Los_Angeles")
	}

	rows, err := db.ListProxies(ctx)
	if err != nil {
		t.Fatalf("ListProxies returned error: %v", err)
	}
	found := false
	for _, p := range rows {
		if p.ID == id {
			found = true
			if p.TestTimezone != "America/Los_Angeles" {
				t.Fatalf("ListProxies test_timezone = %q, want %q", p.TestTimezone, "America/Los_Angeles")
			}
		}
	}
	if !found {
		t.Fatalf("ListProxies did not return proxy %d", id)
	}

	if err := db.UpdateProxyTestResult(ctx, id, "http://pg-tz.example:8080", ProxyTestStatusError, "", "", "", 0); err != nil {
		t.Fatalf("UpdateProxyTestResult(error) returned error: %v", err)
	}
	row, err = db.GetProxy(ctx, id)
	if err != nil {
		t.Fatalf("GetProxy returned error: %v", err)
	}
	if row.TestTimezone != "" {
		t.Fatalf("test_timezone after failed probe = %q, want empty", row.TestTimezone)
	}
}
