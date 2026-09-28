package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The optional PostgreSQL DSN must point at a disposable local test instance.
// Every test uses a fresh schema; it never clears an existing application's data.
func meteringTestDB(t *testing.T, driver string) (*DB, func() *DB) {
	t.Helper()
	dsn, schema := filepath.Join(t.TempDir(), "metering.db"), ""
	if driver == "postgres" {
		raw := os.Getenv("CODEX2API_METERING_TEST_POSTGRES_DSN")
		if raw == "" {
			t.Skip("set CODEX2API_METERING_TEST_POSTGRES_DSN for isolated PostgreSQL tests")
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		schema = fmt.Sprintf("metering_test_%d", time.Now().UnixNano())
		q := u.Query()
		q.Set("search_path", schema+",public")
		q.Set("timezone", "UTC")
		u.RawQuery = q.Encode()
		dsn = u.String()
		admin, err := sql.Open("pgx", raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + quotePostgresIdent(schema) + ` CASCADE`)
			if err != nil {
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
		CacheWrite5mTokens: 30, CacheWrite1hTokens: 40, ImageInputTokens: 10,
		ImageOutputTokens: 5, CachedImageInputTokens: 2, ImageCount: 1,
		DurationMs: 900, FirstTokenMs: 100, Stream: true, Compact: true,
		RequestedServiceTier: "priority", ActualServiceTier: "priority", AttemptIndex: 1,
		ClientIP: "192.0.2.1", ClientUserAgent: "private UA", ErrorMessage: "",
		RequestDiagnostics: `{"private":"` + strings.Repeat("sensitive text ", 1024) + `"}`,
	}
}

func requireMeteringClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Fatalf("amount = %.12f, want %.12f", got, want)
	}
}

func TestUsageMeteringModeParity(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			at := time.Now().UTC().Truncate(time.Minute).Add(-30 * time.Minute)
			start, end := at.Add(-time.Hour), at.Add(time.Hour)
			var baseline map[string]any
			for _, mode := range []string{UsageLogModeFull, UsageLogModeErrors, UsageLogModeOff} {
				t.Run(mode, func(t *testing.T) {
					db, _ := meteringTestDB(t, driver)
					db.SetUsageLogConfig(mode, 1000, 300)
					key, err := db.InsertAPIKey(ctx, "metering", "sk-metering-fixture")
					if err != nil {
						t.Fatal(err)
					}
					account, err := db.InsertAccount(ctx, "metering", "synthetic-refresh", "")
					if err != nil {
						t.Fatal(err)
					}
					input := meteringInput(key, account)
					wantCost := 2 * UsageLogBilledCost(&input)
					for i, status := range []int{200, 200, 500, 499, 500, 200} {
						e := input
						e.StatusCode = status
						if status == 500 {
							e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.InputTokens, e.OutputTokens = 0, 0, 0, 0, 0
							e.CachedTokens, e.CacheWrite5mTokens, e.CacheWrite1hTokens = 0, 0, 0
							e.ImageInputTokens, e.ImageOutputTokens, e.CachedImageInputTokens, e.ImageCount = 0, 0, 0, 0
							e.ErrorMessage = "server_is_overloaded · private upstream payload"
						}
						if i == 4 {
							e.IsRetryAttempt = true
						}
						if i == 5 {
							e.InternalReason = "probe"
							e.APIKeyID = 0
						}
						if err := db.InsertUsageLog(ctx, &e); err != nil {
							t.Fatal(err)
						}
					}
					db.FlushUsageLogs()
					for _, table := range []string{"usage_logs", "usage_metering"} {
						if _, err := db.conn.ExecContext(ctx, `UPDATE `+table+` SET created_at=$1`, db.timeArg(at)); err != nil {
							t.Fatal(err)
						}
					}
					var charged float64
					if err := db.conn.QueryRow(`SELECT quota_used FROM api_keys WHERE id=$1`, key).Scan(&charged); err != nil {
						t.Fatal(err)
					}
					requireMeteringClose(t, charged, wantCost)
					for _, window := range []time.Duration{5 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour} {
						got, err := db.GetAPIKeyWindowUsage(ctx, key, window)
						if err != nil {
							t.Fatal(err)
						}
						if got.Requests != 4 || got.Tokens != 2800 {
							t.Fatalf("window %v: %+v", window, got)
						}
						requireMeteringClose(t, got.UserBilled, charged)
					}
					// Exercise each statistics query family; compare all numeric fields to full mode.
					snapshot := map[string]any{}
					var tokenTotals [12]int64
					if err := db.conn.QueryRow(`SELECT SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens),
						SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens),
						SUM(cache_write_5m_tokens), SUM(cache_write_1h_tokens), SUM(image_input_tokens),
						SUM(image_output_tokens), SUM(cached_image_input_tokens)
						FROM usage_metered_events WHERE api_key_id=$1 AND status_code=200`, key).Scan(
						&tokenTotals[0], &tokenTotals[1], &tokenTotals[2], &tokenTotals[3], &tokenTotals[4], &tokenTotals[5],
						&tokenTotals[6], &tokenTotals[7], &tokenTotals[8], &tokenTotals[9], &tokenTotals[10], &tokenTotals[11]); err != nil {
						t.Fatal(err)
					}
					if want := [12]int64{2400, 400, 2800, 2400, 400, 120, 200, 60, 80, 20, 10, 4}; tokenTotals != want {
						t.Fatalf("token dimensions = %v, want %v", tokenTotals, want)
					}
					snapshot["token_dimensions"] = tokenTotals
					capture := func(name string, value any, err error) {
						t.Helper()
						if err != nil {
							t.Fatalf("%s: %v", name, err)
						}
						snapshot[name] = value
					}
					stats, err := db.GetUsageStats(ctx, start, end, "")
					capture("stats", stats, err)
					if stats.TotalTokens != 2800 || stats.TotalRequests != 4 {
						t.Fatalf("stats: %+v", stats)
					}
					requireMeteringClose(t, stats.TotalUserBilled, charged)
					channel, err := db.GetUsageStats(ctx, start, end, "codex")
					capture("channel", channel, err)
					chart, err := db.GetChartAggregation(ctx, start, end, 5, "")
					capture("chart", chart, err)
					accountStats, err := db.GetAccountUsageStats(ctx, account, 0)
					capture("account", accountStats, err)
					counts, err := db.GetAccountRequestCountsByIDs(ctx, []int64{account})
					capture("counts", counts, err)
					tokens, err := db.ListAPIKeyTokenStats(ctx, start, end)
					capture("key_tokens", tokens, err)
					costs, err := db.GetAccountsBilledSince(ctx, map[int64]time.Time{account: start})
					capture("account_costs", costs, err)
					keys, err := db.GetAllAPIKeysWindowCost(ctx, 5*time.Hour)
					capture("key_costs", keys, err)
					accountWindows, err := db.GetAPIKeyAccountWindowsUsage(ctx, key)
					capture("account_windows", accountWindows, err)
					health, err := db.GetAccountsHealthBucketsByIDs(ctx, []int64{account}, end, 12, 10*time.Minute)
					capture("health", health, err)
					self, err := db.GetAPIKeySelfUsageReport(ctx, key, start, end, 1, 25)
					if err != nil {
						t.Fatal(err)
					}
					capture("self_summary", self.Summary, nil)
					capture("self_models", self.Models, nil)
					if mode == UsageLogModeOff && len(self.RecentLogs) != 0 {
						t.Fatal("lightweight entries leaked into detail API")
					}
					logs, err := db.ListRecentUsageLogs(ctx, 100)
					if err != nil {
						t.Fatal(err)
					}
					wantDetails := map[string]int{"full": 5, "errors": 2, "off": 0}[mode]
					if len(logs) != wantDetails {
						t.Fatalf("details = %d, want %d", len(logs), wantDetails)
					}
					if baseline == nil {
						baseline = snapshot
					} else if !reflect.DeepEqual(snapshot, baseline) {
						for name, value := range snapshot {
							if !reflect.DeepEqual(value, baseline[name]) {
								got, _ := json.Marshal(value)
								want, _ := json.Marshal(baseline[name])
								t.Errorf("%s differs: %s; full: %s", name, got, want)
							}
						}
					}
				})
			}
		})
	}
}

func TestUsageMeteringSwitchClearAndRestart(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			db, reopen := meteringTestDB(t, driver)
			key, err := db.InsertAPIKey(ctx, "switch", "sk-metering-switch")
			if err != nil {
				t.Fatal(err)
			}
			input := meteringInput(key, 1)
			for _, mode := range []string{UsageLogModeOff, UsageLogModeFull, UsageLogModeErrors, UsageLogModeOff} {
				db.SetUsageLogConfig(mode, 1000, 300)
				if err := db.InsertUsageLog(ctx, &input); err != nil {
					t.Fatal(err)
				}
			}
			db.FlushUsageLogs()
			if err := db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "metering", UsageMeteringEnabled: true}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := db.ClearUsageLogs(ctx); err != nil {
					t.Fatal(err)
				}
				if err := db.rebuildUsageStatsRollup(ctx); err != nil {
					t.Fatal(err)
				}
				stats, err := db.GetUsageStatsSummary(ctx, time.Time{}, time.Time{}, "")
				if err != nil {
					t.Fatal(err)
				}
				if stats.TotalRequests != 4 || stats.TotalTokens != 5600 {
					t.Fatalf("clear duplicated/lost usage: %+v", stats)
				}
				window, err := db.GetAPIKeyWindowUsage(ctx, key, 5*time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if window.Requests != 3 || window.Tokens != 4200 {
					t.Fatalf("clear erased lightweight metering: %+v", window)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db = reopen()
				if !db.GetUsageMeteringEnabled() {
					t.Fatal("restart lost setting")
				}
			}
			settings, err := db.GetSystemSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			settings.UsageMeteringEnabled = false
			if err := db.UpdateSystemSettings(ctx, settings); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = reopen()
			if db.GetUsageMeteringEnabled() {
				t.Fatal("restart lost explicit disable")
			}
			db.SetUsageLogConfig(UsageLogModeOff, 1000, 300)
			if err := db.InsertUsageLog(ctx, &input); err != nil {
				t.Fatal(err)
			}
			db.FlushUsageLogs()
			window, err := db.GetAPIKeyWindowUsage(ctx, key, 5*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if window.Requests != 3 {
				t.Fatalf("disabled switch should preserve old metering only: %+v", window)
			}
			var charged float64
			if err := db.conn.QueryRow(`SELECT quota_used FROM api_keys WHERE id=$1`, key).Scan(&charged); err != nil {
				t.Fatal(err)
			}
			requireMeteringClose(t, charged, 5*UsageLogBilledCost(&input))
		})
	}
}

func TestUsageMeteringAtomicRollbackAndExactBoundary(t *testing.T) {
	db, _ := meteringTestDB(t, "sqlite")
	ctx := context.Background()
	key, err := db.InsertAPIKey(ctx, "atomic", "sk-metering-atomic")
	if err != nil {
		t.Fatal(err)
	}
	db.SetUsageLogConfig(UsageLogModeOff, 1000, 300)
	input := meteringInput(key, 1)
	if err := db.InsertUsageLog(ctx, &input); err != nil {
		t.Fatal(err)
	}
	db.logMu.Lock()
	batch := append([]usageLogEntry(nil), db.logBuf...)
	db.logBuf = nil
	db.logMu.Unlock()
	if len(batch) != 1 || batch[0].RequestDiagnostics != "" || batch[0].ClientUserAgent != "" || batch[0].ClientIP != "" {
		t.Fatal("disabled details retained diagnostic payload")
	}
	// Fail after the ledger insert, when charging. The transaction must undo both.
	if _, err := db.conn.Exec(`CREATE TRIGGER reject_metering_charge BEFORE UPDATE OF quota_used ON api_keys BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.insertSQLiteUsageLogBatch(ctx, batch); err == nil {
		t.Fatal("expected charge failure")
	}
	var count int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM usage_metering`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial metering committed: %d %v", count, err)
	}
	if _, err := db.conn.Exec(`DROP TRIGGER reject_metering_charge`); err != nil {
		t.Fatal(err)
	}
	if err := db.insertSQLiteUsageLogBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	boundary := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	if _, err := db.conn.Exec(`UPDATE usage_metering SET created_at=$1`, db.timeArg(boundary)); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []time.Duration{0, time.Second} {
		usage, err := db.GetAPIKeyUsageSince(ctx, key, boundary.Add(delta))
		if err != nil {
			t.Fatal(err)
		}
		want := int64(1)
		if delta > 0 {
			want = 0
		}
		if usage.Requests != want {
			t.Fatalf("exact boundary %v: %+v", delta, usage)
		}
	}
	var charged float64
	if err := db.conn.QueryRow(`SELECT quota_used FROM api_keys WHERE id=$1`, key).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	requireMeteringClose(t, charged, UsageLogBilledCost(&input))
}

func TestUsageMeteringMigrationDefaultsOn(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db, reopen := meteringTestDB(t, driver)
			ctx := context.Background()
			if err := db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "existing installation"}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.conn.Exec(`ALTER TABLE system_settings DROP COLUMN usage_metering_enabled`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = reopen()
			settings, err := db.GetSystemSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !db.GetUsageMeteringEnabled() || !settings.UsageMeteringEnabled || settings.SiteName != "existing installation" {
				t.Fatal("migration did not default on or changed existing settings")
			}
		})
	}
}

func TestUsageMeteringChunkedBatch(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db, _ := meteringTestDB(t, driver)
			ctx := context.Background()
			batch := make([]usageLogEntry, 1001)
			for i := range batch {
				batch[i] = usageLogEntry{StoreMetering: true, StatusCode: 200, TotalTokens: 100, Channel: "codex"}
			}
			var err error
			if driver == "sqlite" {
				err = db.insertSQLiteUsageLogBatch(ctx, batch)
			} else {
				err = db.batchInsertLogs(ctx, batch)
			}
			if err != nil {
				t.Fatal(err)
			}
			stats, err := db.GetUsageStatsSummary(ctx, time.Time{}, time.Time{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if stats.TotalRequests != 1001 || stats.TotalTokens != 100100 {
				t.Fatalf("chunking lost usage: %+v", stats)
			}
		})
	}
}
