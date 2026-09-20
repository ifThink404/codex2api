package database

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPostgresCodexTurnStateRenewalAndHistory(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL DSN not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now().Unix()
	row := CodexTurnStateTemplate{AccountID: now, Model: "merge-test", Value: "synthetic-fixture", IssuedAt: now - 3000, UpdatedAt: now}
	if err := db.SaveCodexTurnStateTemplate(ctx, row); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimCodexTurnStateRenewal(ctx, row, now, now+90)
	if err != nil || claim != 1 {
		t.Fatalf("claim=%d err=%v", claim, err)
	}
	if again, err := db.ClaimCodexTurnStateRenewal(ctx, row, now, now+90); err != nil || again != 0 {
		t.Fatalf("duplicate claim=%d err=%v", again, err)
	}
	if err := db.RecordCodexTurnStateRenewalProxy(ctx, row, 1, 7, "route-hash"); err != nil {
		t.Fatal(err)
	}
	hashes, err := db.CodexTurnStateRenewalProxyHashes(ctx, row)
	if err != nil || !hashes["route-hash"] {
		t.Fatal("proxy retry route not recorded")
	}
	id, err := db.StartCodexTurnStateHistory(ctx, CodexTurnStateRenewalRecord{AccountID: row.AccountID, Model: row.Model, AccountName: "merge-test", Attempt: 1, MaxAttempts: 10, ProxyURL: "socks5://user:test-only@proxy.test:1080", StartedAt: now * 1000, ExpiresBefore: (now + 600) * 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishCodexTurnStateHistory(ctx, id, "failed", "probe_failed", now*1000+10, 10, 0); err != nil {
		t.Fatal(err)
	}
	page, err := db.ListCodexTurnStateHistory(ctx, 1, 20, CodexTurnStateHistoryFilter{AccountID: row.AccountID, Status: "failed"})
	if err != nil || page.Total != 1 || page.Records[0].ProxyURL != "socks5://proxy.test:1080" {
		t.Fatalf("history=%+v err=%v", page, err)
	}
	if err := db.FinishCodexTurnStateRenewal(ctx, row, 1, now+10); err != nil {
		t.Fatal(err)
	}
	if claim, err := db.ClaimCodexTurnStateRenewal(ctx, row, now+10, now+100); err != nil || claim != 2 {
		t.Fatalf("retry claim=%d err=%v", claim, err)
	}
	if err := db.DeleteCodexTurnStateTemplate(ctx, row.AccountID, row.Model); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneCodexTurnStateRenewals(ctx); err != nil {
		t.Fatal(err)
	}
}
