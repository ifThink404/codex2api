package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Both branches extended the positional INSERT and four readers. Pin the union,
// including NULL mismatch, rather than testing either feature independently.
func TestUsageAuditMergeRoundTrip(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
			if driver == "postgres" && dsn == "" {
				t.Skip("PostgreSQL DSN not set")
			}
			if driver == "sqlite" {
				dsn = filepath.Join(t.TempDir(), "merge.db")
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			marker := fmt.Sprintf("audit-merge-%d", time.Now().UnixNano())
			db.SetUsageLogConfig(UsageLogModeFull, 100, 300)
			for i := 0; i < 3; i++ {
				length, mismatch := 292+i, i == 0
				var audit *bool
				model := ""
				if i < 2 {
					audit = &mismatch
					model = strings.Repeat("m", 150)
				}
				err = db.InsertUsageLog(ctx, &UsageLogInput{RequestID: fmt.Sprintf("%s-%d", marker, i), Endpoint: marker, Model: "requested", EffectiveModel: "sent", UpstreamResponseModel: model, UpstreamModelMismatch: audit, WindowNumber: fmt.Sprint(i + 1), TurnStateLength: &length, TurnStateEcho: "same", TurnStateStripped: i == 0, StatusCode: 200, ReasoningEffort: "high"})
				if err != nil {
					t.Fatal(err)
				}
			}
			db.FlushUsageLogs()
			start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			filter := UsageLogFilter{Start: start, End: end, Query: marker, Page: 1, PageSize: 50}
			check := func(logs []*UsageLog, err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
				seen := 0
				for _, row := range logs {
					if row.Endpoint != marker {
						continue
					}
					seen++
					var i int
					if _, err := fmt.Sscanf(strings.TrimPrefix(row.RequestID, marker+"-"), "%d", &i); err != nil {
						t.Fatal(err)
					}
					if row.WindowNumber != fmt.Sprint(i+1) || row.TurnStateLength == nil || *row.TurnStateLength != 292+i || row.TurnStateEcho != "same" || row.TurnStateStripped != (i == 0) || row.ReasoningEffort != "high" {
						t.Fatalf("fork audit fields misaligned: %+v", row)
					}
					if i == 2 {
						if row.UpstreamModelMismatch != nil || row.UpstreamResponseModel != "" {
							t.Fatal("unknown model must stay unknown")
						}
					} else if row.UpstreamModelMismatch == nil || *row.UpstreamModelMismatch != (i == 0) || len(row.UpstreamResponseModel) != 150 {
						t.Fatal("upstream audit fields misaligned")
					}
				}
				if seen != 3 {
					t.Fatalf("got %d rows, want 3", seen)
				}
			}
			check(db.ListRecentUsageLogs(ctx, 5000))
			check(db.ListUsageLogsByTimeRange(ctx, start, end))
			check(db.ListUsageLogsByFilter(ctx, filter))
			page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				t.Fatal(err)
			}
			check(page.Logs, nil)
			mismatch := true
			filter.UpstreamModelMismatchOnly, filter.TurnState = &mismatch, "received"
			page, err = db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil || page.Total != 1 {
				t.Fatalf("combined filters: page=%+v err=%v", page, err)
			}
			if driver == "postgres" {
				if _, err := db.conn.ExecContext(ctx, "DELETE FROM usage_logs WHERE endpoint = $1", marker); err != nil {
					t.Fatal(err)
				}
				if _, err := db.conn.ExecContext(ctx, "ALTER TABLE usage_logs ALTER COLUMN upstream_response_model TYPE VARCHAR(100)"); err != nil {
					t.Fatal(err)
				}
				if err := db.migrate(ctx); err != nil {
					t.Fatal(err)
				}
				var width int
				if err := db.conn.QueryRowContext(ctx, "SELECT character_maximum_length FROM information_schema.columns WHERE table_schema = current_schema() AND table_name='usage_logs' AND column_name='upstream_response_model'").Scan(&width); err != nil || width != 200 {
					t.Fatalf("upgraded width=%d err=%v", width, err)
				}
			}
		})
	}
}
