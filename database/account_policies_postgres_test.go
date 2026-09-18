package database

import (
	"context"
	"os"
	"testing"
)

// Run against an empty disposable database, as for the other PostgreSQL tests:
//
//	docker run -d --rm --name c2a-pg-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=codex2api_test -p 55432:5432 postgres:16
//	CODEX2API_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/codex2api_test?sslmode=disable' \
//	    go test ./database/ -run TestPostgresAccountPolicyColumns -count=1
//
// 生产跑的是 PostgreSQL，而账号策略三列只在 SQLite 上测过。这里覆盖只有 pq 驱动才
// 算数的那几处：VARCHAR(16) NOT NULL DEFAULT 'inherit' 的 ADD COLUMN 回填、四处
// SELECT/Scan 的列位对齐（SQLite 的弱类型会把错位吞掉，pq 不会），以及
// AFTER UPDATE OF 列清单——三列不在清单里的话，策略改动永远不会进 scheduler_outbox。
func TestPostgresAccountPolicyColumns(t *testing.T) {
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

	t.Run("column definition", func(t *testing.T) { postgresAccountPolicyColumnDefinition(ctx, t, db) })
	t.Run("column backfill", func(t *testing.T) { postgresAccountPolicyColumnBackfill(ctx, t, db) })
	t.Run("round trip", func(t *testing.T) { postgresAccountPolicyRoundTrip(ctx, t, db) })
	t.Run("outbox trigger", func(t *testing.T) { postgresAccountPolicyOutboxTrigger(ctx, t, db) })
}

func postgresAccountPolicyColumnDefinition(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	for _, column := range []string{"prompt_filter_policy", "egress_policy", "session_guards_policy"} {
		var dataType, columnDefault, isNullable string
		var maxLength int
		err := db.conn.QueryRowContext(ctx, `
			SELECT data_type, COALESCE(column_default, ''), is_nullable, COALESCE(character_maximum_length, 0)
			FROM information_schema.columns WHERE table_name = 'accounts' AND column_name = $1`, column).
			Scan(&dataType, &columnDefault, &isNullable, &maxLength)
		if err != nil {
			t.Fatalf("describe %s: %v", column, err)
		}
		if dataType != "character varying" || maxLength != 16 {
			t.Fatalf("%s type = %s(%d), want character varying(16)", column, dataType, maxLength)
		}
		if isNullable != "NO" {
			t.Fatalf("%s is_nullable = %s, want NO", column, isNullable)
		}
		if columnDefault != "'inherit'::character varying" {
			t.Fatalf("%s default = %q, want 'inherit'::character varying", column, columnDefault)
		}
	}
}

func postgresAccountPolicyColumnBackfill(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	// 老库没有这三列：删掉后重跑 migrate，存量行必须回填成 inherit 而不是 NULL。
	id, err := db.InsertAccount(ctx, "pg-policy-backfill", "rt_pg_backfill", "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	defer func() { _, _ = db.conn.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, id) }()

	// 触发器的 AFTER UPDATE OF 清单引用了这三列，不先删触发器 PostgreSQL 会拒绝
	// DROP COLUMN（2BP01）。真实升级路径同样靠 migrate 重建触发器。
	if _, err := db.conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS scheduler_outbox_accounts_update ON accounts`); err != nil {
		t.Fatalf("drop outbox update trigger: %v", err)
	}
	for _, column := range []string{"prompt_filter_policy", "egress_policy", "session_guards_policy"} {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN IF EXISTS `+column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatalf("re-run migrate: %v", err)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.PromptFilterPolicy != "inherit" || row.EgressPolicy != "inherit" || row.SessionGuardsPolicy != "inherit" {
		t.Fatalf("backfilled row = %+v, want all inherit", row)
	}

	// migrate 必须同时把触发器装回来，否则升级后的库永远收不到策略改动事件。
	watermark, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET egress_policy = 'direct' WHERE id = $1`, id); err != nil {
		t.Fatalf("update egress_policy after migrate: %v", err)
	}
	events, err := db.ListSchedulerOutboxEventsAfter(ctx, watermark, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	emitted := false
	for _, event := range events {
		if event.EntityType == "account" && event.EntityID == id {
			emitted = true
		}
	}
	if !emitted {
		t.Fatal("migrate did not reinstall the accounts outbox trigger after the columns were re-added")
	}
}

func postgresAccountPolicyRoundTrip(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	id, err := db.InsertAccount(ctx, "pg-policy-roundtrip", "rt_pg_roundtrip", "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	defer func() { _, _ = db.conn.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, id) }()

	if err := db.UpdateAccountSchedulerMetadata(ctx, id,
		OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{},
		OptionalInt64Slice{}, OptionalStringSlice{}, OptionalInt64Slice{},
		OptionalString{}, nil,
		AccountPolicyUpdate{
			PromptFilterPolicy:  OptionalString{Set: true, Value: "exempt"},
			EgressPolicy:        OptionalString{Set: true, Value: "direct"},
			SessionGuardsPolicy: OptionalString{Set: true, Value: "off"},
		}); err != nil {
		t.Fatalf("UpdateAccountSchedulerMetadata: %v", err)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.PromptFilterPolicy != "exempt" || row.EgressPolicy != "direct" || row.SessionGuardsPolicy != "off" {
		t.Fatalf("GetAccountByID = %+v, want exempt/direct/off", row)
	}

	projected, err := db.ListActiveByIDs(ctx, []int64{id})
	if err != nil || len(projected) != 1 {
		t.Fatalf("ListActiveByIDs = %d rows, err = %v", len(projected), err)
	}
	if projected[0].PromptFilterPolicy != "exempt" || projected[0].EgressPolicy != "direct" || projected[0].SessionGuardsPolicy != "off" {
		t.Fatalf("ListActiveByIDs = %+v, want exempt/direct/off", projected[0])
	}

	listed, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	found := false
	for _, r := range listed {
		if r.ID == id {
			found = true
			if r.PromptFilterPolicy != "exempt" || r.EgressPolicy != "direct" || r.SessionGuardsPolicy != "off" {
				t.Fatalf("ListActive = %+v, want exempt/direct/off", r)
			}
		}
	}
	if !found {
		t.Fatalf("ListActive did not return account %d", id)
	}

	if err := db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	deleted, err := db.ListDeleted(ctx)
	if err != nil {
		t.Fatalf("ListDeleted: %v", err)
	}
	for _, r := range deleted {
		if r.ID == id && (r.PromptFilterPolicy != "exempt" || r.EgressPolicy != "direct" || r.SessionGuardsPolicy != "off") {
			t.Fatalf("ListDeleted = %+v, want exempt/direct/off", r)
		}
	}
}

func postgresAccountPolicyOutboxTrigger(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	id, err := db.InsertAccount(ctx, "pg-policy-outbox", "rt_pg_outbox", "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	defer func() { _, _ = db.conn.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, id) }()

	for _, tc := range []struct {
		column string
		value  string
	}{
		{"prompt_filter_policy", "exempt"},
		{"egress_policy", "direct"},
		{"session_guards_policy", "off"},
	} {
		watermark, err := db.SchedulerOutboxHighWatermark(ctx)
		if err != nil {
			t.Fatalf("watermark before %s: %v", tc.column, err)
		}
		if _, err := db.conn.ExecContext(ctx,
			`UPDATE accounts SET `+tc.column+` = $1 WHERE id = $2`, tc.value, id); err != nil {
			t.Fatalf("update %s: %v", tc.column, err)
		}
		events, err := db.ListSchedulerOutboxEventsAfter(ctx, watermark, 10)
		if err != nil {
			t.Fatalf("list events after %s: %v", tc.column, err)
		}
		emitted := false
		for _, event := range events {
			if event.EntityType == "account" && event.EntityID == id {
				emitted = true
			}
		}
		if !emitted {
			t.Fatalf("changing %s emitted no scheduler_outbox event; the AFTER UPDATE OF list is missing the column", tc.column)
		}
	}
}
