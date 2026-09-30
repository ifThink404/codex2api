package admin

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestLogAgentRetentionPurgesByConfiguredDays(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	h := &Handler{db: db}
	cfg := database.DefaultLogAgentConfig()
	cfg.RetentionDays = 7
	if _, err := db.SaveLogAgentConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, age := range []time.Duration{time.Hour, 8 * 24 * time.Hour} {
		run := &database.LogAgentRun{Source: "usage_logs", Status: database.LogAgentRunStatusSucceeded, CreatedAt: time.Now().Add(-age)}
		if err := db.InsertLogAgentRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := h.purgeExpiredLogAgentRuns(ctx, time.Now())
	if err != nil || deleted != 1 {
		t.Fatalf("purge = %d, %v; want 1 (the run older than 7 days)", deleted, err)
	}
	runs, err := db.ListLogAgentRuns(ctx, database.LogAgentRunFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("remaining runs = %d, %v", len(runs), err)
	}
	if logAgentPurgeInterval != 10*time.Minute {
		t.Fatalf("log agent retention runs every %s, want 10m", logAgentPurgeInterval)
	}
}
