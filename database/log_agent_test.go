package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogAgentSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "log-agent.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	exerciseLogAgentStore(context.Background(), t, db)
}

// CODEX2API_TEST_POSTGRES_DSN 用法见 session_auto_locks_postgres_test.go。
func TestLogAgentPostgres(t *testing.T) {
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
	if _, err := db.conn.ExecContext(ctx, `DROP TABLE IF EXISTS log_agent_runs; DROP TABLE IF EXISTS log_agent_settings`); err != nil {
		t.Fatal(err)
	}
	exerciseLogAgentStore(ctx, t, db)
}

func exerciseLogAgentStore(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	cfg, err := db.LoadLogAgentConfig(ctx)
	if err != nil || cfg != DefaultLogAgentConfig() || cfg.Enabled {
		t.Fatalf("default config = %+v err=%v", cfg, err)
	}
	saved, err := db.SaveLogAgentConfig(ctx, LogAgentConfig{Enabled: true, APIKeyID: 7, Model: " gpt-5 ", MaxInputBytes: 1, TimeoutSeconds: 9999})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.Model != "gpt-5" || saved.MaxInputBytes != 4<<10 || saved.TimeoutSeconds != MaxLogAgentTimeoutSeconds || saved.RetentionDays != DefaultLogAgentRetentionDays {
		t.Fatalf("normalized = %+v", saved)
	}
	if _, err := db.SaveLogAgentConfig(ctx, LogAgentConfig{Enabled: true, APIKeyID: 8, Model: "gpt-5-mini"}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	loaded, err := db.LoadLogAgentConfig(ctx)
	if err != nil || loaded.APIKeyID != 8 || loaded.Model != "gpt-5-mini" || !loaded.Enabled {
		t.Fatalf("loaded = %+v err=%v", loaded, err)
	}

	old := &LogAgentRun{Source: "ops_errors", Status: LogAgentRunStatusFailed, ErrorMessage: "HTTP 503", CreatedAt: time.Now().UTC().Add(-48 * time.Hour)}
	if err := db.InsertLogAgentRun(ctx, old); err != nil || old.ID == 0 {
		t.Fatalf("insert old: id=%d err=%v", old.ID, err)
	}
	fresh := &LogAgentRun{
		Source: "usage_logs", Subject: json.RawMessage(`{"refs":["r1"]}`), Model: "gpt-5", APIKeyID: 8,
		Status: LogAgentRunStatusSucceeded, Findings: json.RawMessage(`{"summary":"ok"}`), ContextStats: json.RawMessage(`not json`),
		RecordCount: 3, InputTokens: 100, OutputTokens: 20, TotalTokens: 120, DurationMs: 1500,
	}
	if err := db.InsertLogAgentRun(ctx, fresh); err != nil || fresh.ID <= old.ID {
		t.Fatalf("insert fresh: id=%d err=%v", fresh.ID, err)
	}
	got, err := db.GetLogAgentRun(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Source != "usage_logs" || string(got.Subject) != `{"refs":["r1"]}` || string(got.Findings) != `{"summary":"ok"}` ||
		string(got.ContextStats) != `{}` || got.TotalTokens != 120 || got.DurationMs != 1500 || got.CreatedAt.IsZero() {
		t.Fatalf("got = %+v", got)
	}
	if _, err := db.GetLogAgentRun(ctx, 999999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing run err = %v", err)
	}
	all, err := db.ListLogAgentRuns(ctx, LogAgentRunFilter{})
	if err != nil || len(all) != 2 || all[0].ID != fresh.ID {
		t.Fatalf("list all = %d err=%v", len(all), err)
	}
	bySource, err := db.ListLogAgentRuns(ctx, LogAgentRunFilter{Source: "ops_errors"})
	if err != nil || len(bySource) != 1 || bySource[0].ID != old.ID {
		t.Fatalf("list by source = %d err=%v", len(bySource), err)
	}
	older, err := db.ListLogAgentRuns(ctx, LogAgentRunFilter{BeforeID: fresh.ID, Limit: 1})
	if err != nil || len(older) != 1 || older[0].ID != old.ID {
		t.Fatalf("paged = %d err=%v", len(older), err)
	}
	deleted, err := db.PurgeLogAgentRuns(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("purge deleted=%d err=%v", deleted, err)
	}
	remaining, err := db.ListLogAgentRuns(ctx, LogAgentRunFilter{})
	if err != nil || len(remaining) != 1 || remaining[0].ID != fresh.ID {
		t.Fatalf("remaining = %d err=%v", len(remaining), err)
	}
}
