package database

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in sink benchmark, deliberately separate from routine correctness tests.
// This measures committed database batches, not upstream/API request capacity.
func TestUsageMeteringPostgresPerformance(t *testing.T) {
	if os.Getenv("CODEX2API_METERING_PERF") != "1" {
		t.Skip("set CODEX2API_METERING_PERF=1 and the isolated PostgreSQL DSN")
	}
	const batchSize, batches = 200, 60
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))
	payload := make([]byte, 2048)
	for i := range payload {
		payload[i] = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"[rng.Intn(62)]
	}
	for sample := 0; sample < 3; sample++ {
		modes := []string{UsageLogModeFull, UsageLogModeOff}
		if sample%2 == 1 {
			modes[0], modes[1] = modes[1], modes[0]
		}
		for _, mode := range modes {
			t.Run(fmt.Sprintf("%d_%s", sample, mode), func(t *testing.T) {
				db, _ := meteringTestDB(t, "postgres")
				db.SetUsageLogConfig(mode, 1000, 300)
				var keys, accounts []int64
				for i := 0; i < 16; i++ {
					key, err := db.InsertAPIKey(ctx, fmt.Sprintf("key-%d", i), fmt.Sprintf("sk-perf-synthetic-%d", i))
					if err != nil {
						t.Fatal(err)
					}
					keys = append(keys, key)
					account, err := db.InsertAccount(ctx, fmt.Sprintf("account-%d", i), fmt.Sprintf("refresh-perf-%d", i), "")
					if err != nil {
						t.Fatal(err)
					}
					accounts = append(accounts, account)
				}
				for i := 0; i < batchSize; i++ {
					input := meteringInput(keys[i%16], accounts[i%16])
					input.RequestDiagnostics = `{"version":1,"fixture":"` + string(payload) + `"}`
					input.ClientUserAgent = strings.Repeat("synthetic-agent ", 16)
					if err := db.InsertUsageLog(ctx, &input); err != nil {
						t.Fatal(err)
					}
				}
				db.logMu.Lock()
				batch := append([]usageLogEntry(nil), db.logBuf...)
				db.logBuf = nil
				db.logMu.Unlock()
				if len(batch) != batchSize {
					t.Fatalf("unexpected benchmark batch size %d", len(batch))
				}
				// Warm all statement paths before measuring the same fixed amount of work.
				if err := db.batchInsertLogs(ctx, batch); err != nil {
					t.Fatal(err)
				}
				var lsn string
				if err := db.conn.QueryRow(`SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn); err != nil {
					t.Fatal(err)
				}
				var sizeBefore, sizeAfter, walBytes int64
				const sizeSQL = `SELECT pg_total_relation_size('usage_logs')+pg_total_relation_size('usage_metering')`
				if err := db.conn.QueryRow(sizeSQL).Scan(&sizeBefore); err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				for i := 0; i < batches; i++ {
					if err := db.batchInsertLogs(ctx, batch); err != nil {
						t.Fatal(err)
					}
				}
				elapsed := time.Since(started)
				if err := db.conn.QueryRow(`SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint`, lsn).Scan(&walBytes); err != nil {
					t.Fatal(err)
				}
				if err := db.conn.QueryRow(sizeSQL).Scan(&sizeAfter); err != nil {
					t.Fatal(err)
				}
				var count, tokens int64
				var billed, charged float64
				if err := db.conn.QueryRow(`SELECT COUNT(*), SUM(total_tokens), SUM(user_billed) FROM usage_metered_events`).Scan(&count, &tokens, &billed); err != nil {
					t.Fatal(err)
				}
				if err := db.conn.QueryRow(`SELECT SUM(quota_used) FROM api_keys`).Scan(&charged); err != nil {
					t.Fatal(err)
				}
				if count != batchSize*(batches+1) || tokens != 1400*count {
					t.Fatalf("lost events: %d %d", count, tokens)
				}
				requireMeteringClose(t, billed, charged)
				// Verify selective window queries can reach indexes through UNION ALL.
				if _, err := db.conn.Exec(`ANALYZE usage_logs; ANALYZE usage_metering`); err != nil {
					t.Fatal(err)
				}
				var plan string
				if err := db.conn.QueryRow(`EXPLAIN (FORMAT JSON) SELECT SUM(total_tokens) FROM usage_metered_events WHERE api_key_id=-1000 AND created_at >= NOW()-INTERVAL '5 hours'`).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				expectedIndex := "idx_usage_metering_key_created"
				if mode == UsageLogModeFull {
					expectedIndex = "idx_usage_logs_api_key_created_at"
				}
				if !strings.Contains(plan, "Index Cond") || !strings.Contains(plan, expectedIndex) {
					t.Fatalf("metering window lost index path: %s", plan)
				}
				result, _ := json.Marshal(map[string]any{
					"sample": sample, "mode": mode, "rows": batchSize * batches, "batch_size": batchSize,
					"diagnostic_bytes": len(payload), "keys": len(keys), "writer_concurrency": 1,
					"seconds": elapsed.Seconds(), "rows_per_second": float64(batchSize*batches) / elapsed.Seconds(),
					"wal_bytes": walBytes, "relation_growth_bytes": sizeAfter - sizeBefore,
					"bill_tokens_verified": true, "window_index_verified": true,
				})
				t.Logf("METERING_PERF %s", result)
			})
		}
	}
}
