package database

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PostgreSQL runs only with CODEX2API_TEST_POSTGRES_DSN pointing at a
// disposable instance; every test uses a fresh schema and drops it afterwards.
func meteringTestDB(t *testing.T, driver string) (*DB, func() *DB) {
	t.Helper()
	dsn, schema := filepath.Join(t.TempDir(), "metering.db"), ""
	if driver == "postgres" {
		raw := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
		if raw == "" {
			t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		schema = fmt.Sprintf("metering_test_%d", time.Now().UnixNano())
		q := u.Query()
		q.Set("search_path", schema)
		q.Set("timezone", "UTC")
		u.RawQuery = q.Encode()
		dsn = u.String()
		admin, err := sql.Open("pgx", raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(`CREATE SCHEMA ` + quotePostgresIdent(schema)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + quotePostgresIdent(schema) + ` CASCADE`); err != nil {
				t.Error(err)
			}
			admin.Close()
		})
	}
	open := func() *DB {
		t.Helper()
		db, err := New(driver, dsn, schema)
		if err != nil {
			t.Fatal(err)
		}
		if driver == "postgres" {
			// The PostgreSQL migration does not create these turn-state detail
			// columns on a fresh schema (long-lived databases already have them);
			// add them so detail inserts work in this isolated schema.
			if _, err := db.conn.Exec(`ALTER TABLE usage_logs
				ADD COLUMN IF NOT EXISTS turn_state_overridden BOOLEAN DEFAULT FALSE,
				ADD COLUMN IF NOT EXISTS turn_state_rewrite_note TEXT DEFAULT '',
				ADD COLUMN IF NOT EXISTS injected_turn_state TEXT DEFAULT '',
				ADD COLUMN IF NOT EXISTS upstream_turn_state TEXT DEFAULT ''`); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			select {
			case <-db.logStop:
			default:
				db.Close()
			}
		})
		return db
	}
	return open(), open
}

func meteringInput(key, account int64) UsageLogInput {
	return UsageLogInput{
		AccountID: account, APIKeyID: key, APIKeyName: "metering", APIKeyMasked: "sk-test…",
		Channel: "codex", Endpoint: "/v1/responses", InboundEndpoint: "/v1/responses",
		Model: "gpt-5.4", EffectiveModel: "gpt-5.4", StatusCode: 200,
		PromptTokens: 1200, CompletionTokens: 200, TotalTokens: 1400,
		InputTokens: 1200, OutputTokens: 200, ReasoningTokens: 60, CachedTokens: 100,
		CacheWrite5mTokens: 30, CacheWrite1hTokens: 40,
		DurationMs: 900, FirstTokenMs: 100, Stream: true,
		ClientIP: "192.0.2.1", ClientUserAgent: "private UA",
	}
}

type meteringSnapshot struct {
	requests, tokens, keyRequests, keyTokens, accountRequests, accountTokens int64
	userBilled, keyBilled, accountBilled                                     float64
}

func takeMeteringSnapshot(t *testing.T, db *DB, key, account int64, start, end time.Time) meteringSnapshot {
	t.Helper()
	ctx := context.Background()
	stats, err := db.GetUsageStats(ctx, start, end, "")
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	window, err := db.GetAPIKeyUsageSince(ctx, key, start)
	if err != nil {
		t.Fatalf("GetAPIKeyUsageSince: %v", err)
	}
	accounts, err := db.GetAccountTimeRangeUsage(ctx, start)
	if err != nil {
		t.Fatalf("GetAccountTimeRangeUsage: %v", err)
	}
	snapshot := meteringSnapshot{
		requests: stats.TotalRequests, tokens: stats.TotalTokens, userBilled: stats.TotalUserBilled,
		keyRequests: window.Requests, keyTokens: window.Tokens, keyBilled: window.UserBilled,
	}
	if usage := accounts[account]; usage != nil {
		snapshot.accountRequests, snapshot.accountTokens, snapshot.accountBilled = usage.Requests, usage.Tokens, usage.AccountBilled
	}
	return snapshot
}

func requireMeteringSnapshot(t *testing.T, got, want meteringSnapshot) {
	t.Helper()
	close := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if got.requests != want.requests || got.tokens != want.tokens || got.keyRequests != want.keyRequests ||
		got.keyTokens != want.keyTokens || got.accountRequests != want.accountRequests || got.accountTokens != want.accountTokens ||
		!close(got.userBilled, want.userBilled) || !close(got.keyBilled, want.keyBilled) || !close(got.accountBilled, want.accountBilled) {
		t.Fatalf("metering snapshot = %+v, want %+v", got, want)
	}
}

func countRows(t *testing.T, db *DB, table string) int {
	t.Helper()
	var count int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func TestUsageMeteringModeParity(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			var baseline *meteringSnapshot
			for _, mode := range []string{UsageLogModeFull, UsageLogModeErrors, UsageLogModeOff} {
				t.Run(mode, func(t *testing.T) {
					db, _ := meteringTestDB(t, driver)
					db.SetUsageLogConfig(mode, 1000, 300)
					key, err := db.InsertAPIKey(ctx, "metering", "sk-metering-"+mode)
					if err != nil {
						t.Fatal(err)
					}
					account, err := db.InsertAccount(ctx, "metering", "synthetic-refresh", "")
					if err != nil {
						t.Fatal(err)
					}
					for _, status := range []int{200, 200, 500, 499, 200} {
						input := meteringInput(key, account)
						input.StatusCode = status
						if status >= 400 {
							input.ErrorMessage = "upstream failure"
						}
						if err := db.InsertUsageLog(ctx, &input); err != nil {
							t.Fatal(err)
						}
					}
					db.FlushUsageLogs()
					logs, metered := countRows(t, db, "usage_logs"), countRows(t, db, "usage_metering")
					wantLogs := map[string]int{UsageLogModeFull: 5, UsageLogModeErrors: 2, UsageLogModeOff: 0}[mode]
					if logs != wantLogs || logs+metered != 5 {
						t.Fatalf("usage_logs=%d usage_metering=%d, want %d detail rows and 5 events in exactly one table", logs, metered, wantLogs)
					}
					snapshot := takeMeteringSnapshot(t, db, key, account, start, end)
					if snapshot.requests != 4 || snapshot.tokens != 4*1400 || snapshot.keyTokens == 0 {
						t.Fatalf("snapshot %+v does not count the four non-cancelled events", snapshot)
					}
					if baseline == nil {
						baseline = &snapshot
					} else {
						requireMeteringSnapshot(t, snapshot, *baseline)
					}
				})
			}
		})
	}
}

func TestUsageMeteringDisabledKeepsLegacyBehaviour(t *testing.T) {
	ctx := context.Background()
	db, _ := meteringTestDB(t, "sqlite")
	db.SetUsageLogConfig(UsageLogModeOff, 1000, 300)
	db.SetUsageMeteringEnabled(false)
	input := meteringInput(0, 1)
	if err := db.InsertUsageLog(ctx, &input); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	if logs, metered := countRows(t, db, "usage_logs"), countRows(t, db, "usage_metering"); logs != 0 || metered != 0 {
		t.Fatalf("usage_logs=%d usage_metering=%d, want nothing stored", logs, metered)
	}
}

func TestUsageMeteringSurvivesUsageLogClear(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			db, _ := meteringTestDB(t, driver)
			key, err := db.InsertAPIKey(ctx, "metering", "sk-metering-clear")
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{UsageLogModeFull, UsageLogModeOff} {
				db.SetUsageLogConfig(mode, 1000, 300)
				input := meteringInput(key, 7)
				if err := db.InsertUsageLog(ctx, &input); err != nil {
					t.Fatal(err)
				}
				db.FlushUsageLogs()
			}
			before, err := db.GetUsageStats(ctx, time.Time{}, time.Time{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.ClearUsageLogs(ctx); err != nil {
				t.Fatalf("ClearUsageLogs: %v", err)
			}
			after, err := db.GetUsageStats(ctx, time.Time{}, time.Time{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if before.TotalRequests != 2 || after.TotalRequests != before.TotalRequests || after.TotalTokens != before.TotalTokens ||
				math.Abs(after.TotalUserBilled-before.TotalUserBilled) > 1e-9 {
				t.Fatalf("cumulative totals changed across clear: before=%d/%d/%f after=%d/%d/%f",
					before.TotalRequests, before.TotalTokens, before.TotalUserBilled, after.TotalRequests, after.TotalTokens, after.TotalUserBilled)
			}
			// The lightweight row is still live for sliding windows.
			window, err := db.GetAPIKeyUsageSince(ctx, key, start)
			if err != nil {
				t.Fatal(err)
			}
			if window.Requests != 1 || countRows(t, db, "usage_metering") != 1 || countRows(t, db, "usage_logs") != 0 {
				t.Fatalf("window=%+v after clear, want the metered event only", window)
			}
			_ = end
		})
	}
}

func TestUsageMeteringSwitchPersists(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db, reopen := meteringTestDB(t, driver)
			if !db.GetUsageMeteringEnabled() {
				t.Fatal("lightweight metering must default to enabled")
			}
			if err := db.SaveUsageMeteringEnabled(context.Background(), false); err != nil {
				t.Fatalf("SaveUsageMeteringEnabled: %v", err)
			}
			db.Close()
			if reopen().GetUsageMeteringEnabled() {
				t.Fatal("disabled switch was not restored after restart")
			}
		})
	}
}

// Every statistics query redirected to usage_metered_events must stay valid
// SQL on both dialects and see the lightweight row.
func TestUsageMeteringStatisticsQueriesReadLightweightRows(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			db, _ := meteringTestDB(t, driver)
			db.SetUsageLogConfig(UsageLogModeOff, 1000, 300)
			key, err := db.InsertAPIKey(ctx, "metering", "sk-metering-queries")
			if err != nil {
				t.Fatal(err)
			}
			account, err := db.InsertAccount(ctx, "metering", "synthetic-refresh", "")
			if err != nil {
				t.Fatal(err)
			}
			input := meteringInput(key, account)
			if err := db.InsertUsageLog(ctx, &input); err != nil {
				t.Fatal(err)
			}
			db.FlushUsageLogs()
			now := time.Now()
			start, end := now.Add(-time.Hour), now.Add(time.Hour)
			ids := []int64{account}
			check := func(name string, err error, seen bool) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !seen {
					t.Fatalf("%s did not include the lightweight metering row", name)
				}
			}
			counts, err := db.GetAccountRequestCountsByIDs(ctx, ids)
			check("GetAccountRequestCountsByIDs", err, counts[account] != nil && counts[account].SuccessCount == 1)
			all, err := db.GetAccountRequestCounts(ctx)
			check("GetAccountRequestCounts", err, all[account] != nil && all[account].SuccessCount == 1)
			short, _, err := db.GetAccountUsageWindowsByIDs(ctx, ids, start, start)
			check("GetAccountUsageWindowsByIDs", err, short[account] != nil && short[account].Tokens == 1400)
			shortAll, _, err := db.GetAccountUsageWindows(ctx, start, start)
			check("GetAccountUsageWindows", err, shortAll[account] != nil && shortAll[account].Requests == 1)
			models, err := db.GetAccountModelCountsSinceByIDs(ctx, ids, start)
			check("GetAccountModelCountsSinceByIDs", err, len(models[account]) == 1)
			buckets, err := db.GetAccountsHealthBucketsByIDs(ctx, ids, now.Add(time.Minute), 4, 30*time.Minute)
			seen := false
			for _, bucket := range buckets[account] {
				seen = seen || bucket.Success > 0
			}
			check("GetAccountsHealthBucketsByIDs", err, seen)
			billed, err := db.GetAccountsBilledSince(ctx, map[int64]time.Time{account: start})
			check("GetAccountsBilledSince", err, billed[account] > 0)
			one, err := db.GetAccountBilledSince(ctx, account, start)
			check("GetAccountBilledSince", err, one > 0)
			detail, err := db.GetAccountUsageStats(ctx, account, 1)
			check("GetAccountUsageStats", err, detail != nil)
			windows, err := db.GetAPIKeyAccountWindowsUsage(ctx, key)
			check("GetAPIKeyAccountWindowsUsage", err, len(windows) > 0)
			keyWindows, err := db.GetAPIKeysAccountWindowUsage(ctx, []int64{key}, time.Hour)
			check("GetAPIKeysAccountWindowUsage", err, keyWindows[key][account].Requests == 1)
			tokenStats, err := db.ListAPIKeyTokenStats(ctx, start, end)
			check("ListAPIKeyTokenStats", err, len(tokenStats) == 1)
			accountStats, err := db.ListAPIKeyAccountStats(ctx, key, start, end)
			check("ListAPIKeyAccountStats", err, len(accountStats) == 1)
			lastUsed, err := db.ListAPIKeyLastUsedAt(ctx)
			check("ListAPIKeyLastUsedAt", err, !lastUsed[key].IsZero())
			costs, err := db.GetAllAPIKeysCostSince(ctx, start)
			check("GetAllAPIKeysCostSince", err, costs[key] > 0)
			report, err := db.GetAPIKeySelfUsageReport(ctx, key, start, end, 1, 10)
			check("GetAPIKeySelfUsageReport", err, report != nil)
			stats, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{}, true)
			check("GetUsageStatsFiltered", err, stats.TotalRequests == 1 && stats.TodayRequests == 1)
			filtered, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{Query: "private UA"}, true)
			// Search filters match detail-only columns, so the range fields narrow to detail rows.
			check("GetUsageStatsFiltered(query)", err, filtered.TodayRequests == 0)
			channels, err := db.CountTodayRequestsByChannel(ctx)
			check("CountTodayRequestsByChannel", err, channels["codex"] == 1)
			chart, err := db.GetChartAggregation(ctx, start, end, 5, "")
			check("GetChartAggregation", err, chart != nil)
			traffic, err := db.GetTrafficSnapshot(ctx)
			check("GetTrafficSnapshot", err, traffic != nil)
		})
	}
}
