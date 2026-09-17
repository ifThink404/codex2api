package database

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Run against an empty disposable database, as for the other PostgreSQL tests:
//
//	docker run -d --rm --name c2a-pg-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=codex2api_test -p 55432:5432 postgres:16
//	CODEX2API_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/codex2api_test?sslmode=disable' \
//	    go test ./database/ -run TestPostgresSessionAutoLocksAndSettings -count=1
//
// 生产跑的是 PostgreSQL，而自动锁定与 turn-state 托管的新代码此前只在 SQLite 上测过。
// 这里覆盖只有 pq 驱动才算数的那几处：BIGSERIAL / TIMESTAMPTZ 的建表、system_settings
// 三列的 ADD COLUMN 回填与 133 参数的定位 upsert、VARCHAR(255) 按字符（不是字节）计长，
// 以及 UNIQUE(session_key)。
func TestPostgresSessionAutoLocksAndSettings(t *testing.T) {
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

	t.Run("settings round-trip", func(t *testing.T) { postgresSessionAutoLockSettings(ctx, t, db) })
	t.Run("column backfill", func(t *testing.T) { postgresSessionAutoLockColumnBackfill(ctx, t, db) })
	t.Run("lock lifecycle", func(t *testing.T) { postgresSessionAutoLockLifecycle(ctx, t, db) })
}

func postgresSessionAutoLockSettings(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	// 从头建一行，列默认值才是真的被读出来（也让这个用例可以重复跑）。
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM system_settings WHERE id = 1`); err != nil {
		t.Fatalf("reset settings row: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatalf("seed settings row: %v", err)
	}
	s, err := db.GetSystemSettings(ctx)
	if err != nil || s == nil {
		t.Fatalf("GetSystemSettings = %#v, err = %v", s, err)
	}
	if s.CodexSessionAutoLockEnabled || s.CodexSessionAutoLockThreshold != 3 || !s.CodexTurnStateVaultEnabled {
		t.Fatalf("column defaults = enabled %v threshold %d vault %v; want false 3 true",
			s.CodexSessionAutoLockEnabled, s.CodexSessionAutoLockThreshold, s.CodexTurnStateVaultEnabled)
	}

	// 133 参数的定位 upsert：新增两列后任何一处错位都会在这里表现为写错列或类型报错。
	s.CodexSessionAutoLockEnabled = true
	s.CodexSessionAutoLockThreshold = 7
	s.CodexTurnStateVaultEnabled = false
	if err := db.UpdateSystemSettings(ctx, s); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	s, err = db.GetSystemSettings(ctx)
	if err != nil || s == nil || !s.CodexSessionAutoLockEnabled || s.CodexSessionAutoLockThreshold != 7 || s.CodexTurnStateVaultEnabled {
		t.Fatalf("persisted = %#v, err = %v", s, err)
	}

	s.CodexSessionAutoLockThreshold = 10001
	if err := db.UpdateSystemSettings(ctx, s); err != nil {
		t.Fatalf("UpdateSystemSettings(out of range): %v", err)
	}
	s, err = db.GetSystemSettings(ctx)
	if err != nil || s == nil || s.CodexSessionAutoLockThreshold != 3 {
		t.Fatalf("out-of-range threshold must normalize to 3 on write: %#v, err = %v", s, err)
	}
}

// postgresSessionAutoLockColumnBackfill 走老库升级那条路：三列不存在的既有行经过
// migrate() 之后必须拿到 关 / 3 / 开——尤其托管是 DEFAULT TRUE，回填错了就等于全队
// 静默关掉托管。
func postgresSessionAutoLockColumnBackfill(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	for _, column := range []string{"codex_session_auto_lock_enabled", "codex_session_auto_lock_threshold", "codex_turn_state_vault_enabled"} {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings DROP COLUMN IF EXISTS `+column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatalf("migrate must re-add the three columns: %v", err)
	}
	// 直接读列值，绕开 SELECT 侧的 COALESCE(..., true) 兜网：DEFAULT TRUE 真的回填了
	// 既有行，而不是读的时候才补出来。
	var rawVault *bool
	if err := db.conn.QueryRowContext(ctx, `SELECT codex_turn_state_vault_enabled FROM system_settings WHERE id = 1`).Scan(&rawVault); err != nil {
		t.Fatalf("read the backfilled column: %v", err)
	}
	if rawVault == nil || !*rawVault {
		t.Fatalf("ADD COLUMN ... DEFAULT TRUE must backfill the existing row, got %v", rawVault)
	}
	s, err := db.GetSystemSettings(ctx)
	if err != nil || s == nil {
		t.Fatalf("GetSystemSettings after migrate = %#v, err = %v", s, err)
	}
	if s.CodexSessionAutoLockEnabled || s.CodexSessionAutoLockThreshold != 3 || !s.CodexTurnStateVaultEnabled {
		t.Fatalf("backfilled existing row = enabled %v threshold %d vault %v; want false 3 true",
			s.CodexSessionAutoLockEnabled, s.CodexSessionAutoLockThreshold, s.CodexTurnStateVaultEnabled)
	}
}

func postgresSessionAutoLockLifecycle(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM session_auto_locks`); err != nil {
		t.Fatalf("reset session_auto_locks: %v", err)
	}

	in := SessionAutoLockInput{SessionKey: "pg-sess-1::api-key:9", SessionIDPrefix: "pg-sess-1", APIKeyID: 9, AccountID: 244, ErrorMessage: "server_is_overloaded", Threshold: 3}
	lock, created, err := db.InsertSessionAutoLock(ctx, in)
	if err != nil || !created || lock == nil || lock.ID == 0 || lock.Source != "automatic" {
		t.Fatalf("insert = %#v created=%v err=%v", lock, created, err)
	}
	if lock.LockedAt.IsZero() || lock.CreatedAt.IsZero() {
		t.Fatalf("TIMESTAMPTZ columns did not round-trip: %#v", lock)
	}

	again, created, err := db.InsertSessionAutoLock(ctx, in)
	if err != nil || created || again == nil || again.ID != lock.ID {
		t.Fatalf("re-locking the same session must be idempotent: %#v created=%v err=%v", again, created, err)
	}

	// UNIQUE(session_key) 必须真的落在 PostgreSQL 上：绕开幂等预读直接写重复键要报错。
	_, rawErr := db.conn.ExecContext(ctx,
		`INSERT INTO session_auto_locks (session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at)
		 VALUES ($1, '', 0, 0, '', 3, 'automatic', NOW(), NOW())`, in.SessionKey)
	if rawErr == nil {
		t.Fatal("session_key must be UNIQUE on PostgreSQL")
	}
	if !strings.Contains(strings.ToLower(rawErr.Error()), "duplicate key") {
		t.Fatalf("the duplicate insert failed for the wrong reason: %v", rawErr)
	}

	// VARCHAR(255) 在 PostgreSQL 上按字符计：255 个 CJK（765 字节）必须存得下，
	// 256 个必须被拒——网关那侧的 255 rune 收敛边界就是按这个列宽定的。
	wide := SessionAutoLockInput{SessionKey: strings.Repeat("会", 255), SessionIDPrefix: "pg-wide", Threshold: 3}
	if _, _, err := db.InsertSessionAutoLock(ctx, wide); err != nil {
		t.Fatalf("a 255-rune session key must fit VARCHAR(255): %v", err)
	}
	_, overflowErr := db.conn.ExecContext(ctx,
		`INSERT INTO session_auto_locks (session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at)
		 VALUES ($1, '', 0, 0, '', 3, 'automatic', NOW(), NOW())`, strings.Repeat("话", 256))
	if overflowErr == nil {
		t.Fatal("a 256-rune session key must be rejected by VARCHAR(255)")
	}

	keys, err := db.ListSessionAutoLockKeys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("warm-up keys = %v err=%v", keys, err)
	}
	found := map[string]bool{}
	for _, key := range keys {
		found[key] = true
	}
	if !found[in.SessionKey] || !found[wide.SessionKey] {
		t.Fatalf("warm-up must return both keys verbatim: %v", keys)
	}

	list, err := db.ListSessionAutoLocks(ctx, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %#v err=%v", list, err)
	}
	var listed *SessionAutoLock
	for i := range list {
		if list[i].SessionKey == in.SessionKey {
			listed = &list[i]
		}
	}
	if listed == nil || listed.AccountID != 244 || listed.APIKeyID != 9 || listed.ErrorMessage != "server_is_overloaded" || listed.Threshold != 3 || listed.SessionIDPrefix != "pg-sess-1" {
		t.Fatalf("listed row lost fields: %#v", listed)
	}

	removed, err := db.DeleteSessionAutoLock(ctx, lock.ID)
	if err != nil || removed == nil || removed.SessionKey != in.SessionKey {
		t.Fatalf("delete = %#v err=%v", removed, err)
	}
	if removed, err := db.DeleteSessionAutoLock(ctx, lock.ID); err != nil || removed != nil {
		t.Fatalf("a second delete must be nil, nil (the admin 404 path): %#v %v", removed, err)
	}
	keys, err = db.ListSessionAutoLockKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0] != wide.SessionKey {
		t.Fatalf("keys after delete = %v err=%v", keys, err)
	}
}
