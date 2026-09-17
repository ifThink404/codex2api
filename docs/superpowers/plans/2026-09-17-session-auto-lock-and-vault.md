# Session Auto-Lock, Admission Relay Exemption, Turn-State Vault — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Round two of the Codex session guards: (1) initial-session admission no longer touches sessions served by relay-style accounts, (2) sessions that hit N consecutive final HTTP 500s on official Codex accounts are auto-locked (default off, admin-unlockable), (3) the real `X-Codex-Turn-State` never leaves the gateway — clients get a random substitute that is swapped back on the way out (default on).

**Architecture:** Same patterns as round one. Settings: `CodexTelemetryTimingDebug`-style RuntimeSettings fields persisted in `system_settings`. Auto-lock: a new `session_auto_locks` table (DDL modelled on `prompt_conversation_locks`), an in-process streak map fed from the single usage-log choke point `logUsageForRequest`, a pre-selection lock check at the two request entry points, a small admin list/unlock API, and a runtime-status card. Admission: the existing `checkInitialSessionAdmission` moves behind account selection with a per-request memo. Vault: an in-process map keyed by affinity key; substitution happens at the two per-event write sites (`sanitizeCapacityShedEventForClient` call sites) and the two HTTP header relay functions; restoration happens inside `applyCodexTurnStateEchoPolicy`.

**Tech Stack:** Go 1.26 (stdlib testing, gin, gjson/sjson, crypto/rand), React + TypeScript (`node --test` `.mjs` guards, i18next), PostgreSQL + SQLite.

**Spec:** `docs/superpowers/specs/2026-09-17-session-auto-lock-design.md` (binding). Round-one spec for context: `docs/superpowers/specs/2026-09-17-codex-session-guards-design.md`.

## Global Constraints

- Project CLAUDE.md: `gitnexus_impact({target, direction:"upstream"})` before editing any existing function; `gitnexus_detect_changes()` before every commit; warn on HIGH/CRITICAL.
- Frontend: read `DESIGN.md` before touching `.tsx`; shared `components/ui/` only (`Switch`, `DraftNumberInput`, `SettingField`, `StatusPanel`, `Card`/`CardContent`, `Button`, `Table*`); every new string in `zh.json`, `en.json`, `zh-TW.json`; new settings blocks asserted in `frontend/src/lib/sessionGuards.test.mjs`; `cd frontend && npm test && npm run typecheck`.
- Defaults verbatim: `codex_session_auto_lock_enabled=false`, `codex_session_auto_lock_threshold=3` (1–10000; out of range → 3), `codex_turn_state_vault_enabled=true`.
- Rejection payloads: auto-lock → HTTP 400, code `session_blacklisted`, message `该会话因连续上游错误已被锁定，请新建对话；如需恢复请管理员解锁。`, `details.retry="stop"`; WS → error frame then `ClosePolicyViolation`. Admission rejection unchanged (`codex_session_identity_unavailable`).
- Substitute token format: `c2a-ts-v1.` + 32 lowercase hex chars (16 random bytes). Vault TTL 1 hour. Streak map cap 50000.
- Go tests use stdlib `testing` only. gofmt applies to edited files only (ten upstream files are known gofmt-dirty: `auth/expiry_urgency_test.go auth/proxy_pool.go auth/proxy_pool_integration.go auth/refresh_scheduler.go proxy/continuity_test.go proxy/payload_rules.go proxy/resin.go proxy/wsrelay/message.go proxy/wsrelay/message_test.go admin/codex_fingerprint_mode_test.go`).
- Commit messages `feat(session-guards): ...` / `fix(...)` / `docs(...)`; one commit per task; never `git add -A` (unrelated untracked files `dist/`, `oauth-subscription-status-renewal-requirements.md` exist).
- Verified anchors at HEAD `b266cd3d` are quoted per task; always confirm by grepping the quoted text before editing.

---

### Task 1: Settings persistence + admin API (three new settings)

**Files:**
- Modify: `database/postgres.go` (migration block after `codex_initial_session_max_age_seconds INT DEFAULT 180;`; `SystemSettings` struct after `CodexInitialSessionMaxAgeSeconds    int`; normalizers after `NormalizeCodexInitialSessionMaxAgeSeconds`; `GetSystemSettings` SELECT/Scan tails; post-scan normalization; `UpdateSystemSettings` column list / VALUES / CASE WHEN / ON CONFLICT / args)
- Modify: `database/sqlite.go` (CREATE TABLE list after `codex_initial_session_max_age_seconds INTEGER DEFAULT 180,`; ensure-column slice after the matching entry)
- Modify: `proxy/runtime_config.go` (`RuntimeSettings` after `CodexInitialSessionMaxAgeSeconds int`; `DefaultRuntimeSettings`; `NormalizeRuntimeSettings`; `ApplyRuntimeSettingsFromSystem`)
- Modify: `admin/handler.go` (`settingsResponse` + `updateSettingsReq` after the `CodexInitialSessionMaxAgeSeconds` fields; both response literals; persist literal; apply block after the `CodexInitialSessionMaxAgeSeconds` apply)
- Test: `database/session_auto_lock_settings_test.go`, `admin/session_auto_lock_settings_test.go`

**Interfaces:**
- Produces: `database.SystemSettings` fields `CodexSessionAutoLockEnabled bool`, `CodexSessionAutoLockThreshold int`, `CodexTurnStateVaultEnabled bool`; `database.NormalizeSessionAutoLockThreshold(int) int`; `proxy.RuntimeSettings` fields with the same three names; JSON keys `codex_session_auto_lock_enabled`, `codex_session_auto_lock_threshold`, `codex_turn_state_vault_enabled`.

- [ ] **Step 1: Write the failing DB test**

Create `database/session_auto_lock_settings_test.go`:

```go
package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteSessionAutoLockSettingsRoundtrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "autolock.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatalf("insert defaults: %v", err)
	}
	s, err := db.GetSystemSettings(ctx)
	if err != nil || s == nil {
		t.Fatalf("GetSystemSettings: %#v, err = %v", s, err)
	}
	if s.CodexSessionAutoLockEnabled || s.CodexSessionAutoLockThreshold != 3 || !s.CodexTurnStateVaultEnabled {
		t.Fatalf("defaults = enabled %v threshold %d vault %v; want false 3 true", s.CodexSessionAutoLockEnabled, s.CodexSessionAutoLockThreshold, s.CodexTurnStateVaultEnabled)
	}
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

func TestNormalizeSessionAutoLockThreshold(t *testing.T) {
	for in, want := range map[int]int{0: 3, -1: 3, 1: 1, 3: 3, 10000: 10000, 10001: 3} {
		if got := NormalizeSessionAutoLockThreshold(in); got != want {
			t.Errorf("NormalizeSessionAutoLockThreshold(%d) = %d, want %d", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./database/ -run 'TestSQLiteSessionAutoLockSettingsRoundtrip|TestNormalizeSessionAutoLockThreshold' -count=1`
Expected: compile error — `s.CodexSessionAutoLockEnabled undefined`.

- [ ] **Step 3: database/postgres.go**

After `NormalizeCodexInitialSessionMaxAgeSeconds` add:

```go
// NormalizeSessionAutoLockThreshold bounds the consecutive-500 count that locks
// a session (1..10000, default 3).
func NormalizeSessionAutoLockThreshold(value int) int {
	if value < 1 || value > 10000 {
		return 3
	}
	return value
}
```

In `SystemSettings`, after `CodexInitialSessionMaxAgeSeconds    int  // ...` add:

```go
	CodexSessionAutoLockEnabled   bool // 同一会话连续最终 500 达阈值后自动锁定
	CodexSessionAutoLockThreshold int  // 连续 500 次数阈值，1..10000，默认 3
	CodexTurnStateVaultEnabled    bool // 真实 X-Codex-Turn-State 留在网关，客户端只拿替身
```

Migration block, after the `codex_initial_session_max_age_seconds INT DEFAULT 180;` line add:

```sql
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_session_auto_lock_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_session_auto_lock_threshold INT DEFAULT 3;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_turn_state_vault_enabled BOOLEAN DEFAULT TRUE;
```

`GetSystemSettings` SELECT: change the last line `COALESCE(codex_initial_session_max_age_seconds, 180)` to

```sql
		       COALESCE(codex_initial_session_max_age_seconds, 180),
		       COALESCE(codex_session_auto_lock_enabled, false),
		       COALESCE(codex_session_auto_lock_threshold, 3),
		       COALESCE(codex_turn_state_vault_enabled, true)
```

Scan: after `&s.CodexInitialSessionMaxAgeSeconds,` add `&s.CodexSessionAutoLockEnabled,`, `&s.CodexSessionAutoLockThreshold,`, `&s.CodexTurnStateVaultEnabled,`. After the post-scan line `s.CodexInitialSessionMaxAgeSeconds = NormalizeCodexInitialSessionMaxAgeSeconds(...)` add `s.CodexSessionAutoLockThreshold = NormalizeSessionAutoLockThreshold(s.CodexSessionAutoLockThreshold)`.

`UpdateSystemSettings`: column list — change `codex_initial_session_max_age_seconds\n\t\t\t\t\t)` to `codex_initial_session_max_age_seconds,\n\t\t\t\t\tcodex_session_auto_lock_enabled,\n\t\t\t\t\tcodex_session_auto_lock_threshold,\n\t\t\t\t\tcodex_turn_state_vault_enabled\n\t\t\t\t\t)`; VALUES append `, $129, $130, $131` after `$128`; CASE WHEN `$129` → `$132` and `$130` → `$133`; ON CONFLICT: change the last SET line `codex_initial_session_max_age_seconds = EXCLUDED.codex_initial_session_max_age_seconds` to

```sql
					codex_initial_session_max_age_seconds = EXCLUDED.codex_initial_session_max_age_seconds,
					codex_session_auto_lock_enabled = EXCLUDED.codex_session_auto_lock_enabled,
					codex_session_auto_lock_threshold = EXCLUDED.codex_session_auto_lock_threshold,
					codex_turn_state_vault_enabled = EXCLUDED.codex_turn_state_vault_enabled
```

Args: after `NormalizeCodexInitialSessionMaxAgeSeconds(s.CodexInitialSessionMaxAgeSeconds),` insert `s.CodexSessionAutoLockEnabled,`, `NormalizeSessionAutoLockThreshold(s.CodexSessionAutoLockThreshold),`, `s.CodexTurnStateVaultEnabled,` (before the two `Preserve...` args). Verify: `sed -n '2990,3280p' database/postgres.go | grep -n 'CASE WHEN'` shows only `$132`/`$133`; exactly 133 args.

- [ ] **Step 4: database/sqlite.go**

CREATE TABLE list, after `codex_initial_session_max_age_seconds INTEGER DEFAULT 180,` add:

```sql
					codex_session_auto_lock_enabled INTEGER DEFAULT 0,
					codex_session_auto_lock_threshold INTEGER DEFAULT 3,
					codex_turn_state_vault_enabled INTEGER DEFAULT 1,
```

Ensure-column slice, after `{"system_settings", "codex_initial_session_max_age_seconds", "INTEGER DEFAULT 180"},` add:

```go
		{"system_settings", "codex_session_auto_lock_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_session_auto_lock_threshold", "INTEGER DEFAULT 3"},
		{"system_settings", "codex_turn_state_vault_enabled", "INTEGER DEFAULT 1"},
```

- [ ] **Step 5: proxy/runtime_config.go**

After `CodexInitialSessionMaxAgeSeconds int` add:

```go
	// CodexSessionAutoLockEnabled 同一会话连续最终 500 达阈值后自动锁定（默认关）。
	CodexSessionAutoLockEnabled bool
	// CodexSessionAutoLockThreshold 连续 500 次数阈值，1..10000，默认 3。
	CodexSessionAutoLockThreshold int
	// CodexTurnStateVaultEnabled 真实 X-Codex-Turn-State 留在网关，客户端只拿替身（默认开）。
	CodexTurnStateVaultEnabled bool
```

`DefaultRuntimeSettings()` after `CodexInitialSessionMaxAgeSeconds:    180,` add `CodexSessionAutoLockEnabled: false,`, `CodexSessionAutoLockThreshold: 3,`, `CodexTurnStateVaultEnabled: true,`. In `NormalizeRuntimeSettings`, after the `CodexInitialSessionMaxAgeSeconds` normalization add `settings.CodexSessionAutoLockThreshold = database.NormalizeSessionAutoLockThreshold(settings.CodexSessionAutoLockThreshold)`. In `ApplyRuntimeSettingsFromSystem` after the `next.CodexInitialSessionMaxAgeSeconds = ...` line add:

```go
		next.CodexSessionAutoLockEnabled = settings.CodexSessionAutoLockEnabled
		next.CodexSessionAutoLockThreshold = database.NormalizeSessionAutoLockThreshold(settings.CodexSessionAutoLockThreshold)
		next.CodexTurnStateVaultEnabled = settings.CodexTurnStateVaultEnabled
```

- [ ] **Step 6: Write the failing admin test**

Create `admin/session_auto_lock_settings_test.go` (reuse the `newSettingsTestHandler` helper already defined in `admin/session_guard_settings_test.go`):

```go
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestSessionAutoLockAndVaultSettingsRoundtrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newSettingsTestHandler(t)
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
		h.GetSettings(c)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode (%d): %v", rec.Code, err)
		}
		return out
	}
	put := func(body string) int {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		return rec.Code
	}
	initial := get()
	if initial["codex_session_auto_lock_enabled"] != false || initial["codex_session_auto_lock_threshold"] != float64(3) || initial["codex_turn_state_vault_enabled"] != true {
		t.Fatalf("defaults: %v", initial)
	}
	if code := put(`{"codex_session_auto_lock_enabled":true,"codex_session_auto_lock_threshold":5,"codex_turn_state_vault_enabled":false}`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	after := get()
	if after["codex_session_auto_lock_enabled"] != true || after["codex_session_auto_lock_threshold"] != float64(5) || after["codex_turn_state_vault_enabled"] != false {
		t.Fatalf("persisted: %v", after)
	}
	rt := proxy.CurrentRuntimeSettings()
	if !rt.CodexSessionAutoLockEnabled || rt.CodexSessionAutoLockThreshold != 5 || rt.CodexTurnStateVaultEnabled {
		t.Fatalf("runtime not hot-applied: %#v", rt)
	}
	if code := put(`{"codex_session_auto_lock_threshold":0}`); code != http.StatusOK {
		t.Fatalf("PUT threshold 0 = %d", code)
	}
	if get()["codex_session_auto_lock_threshold"] != float64(3) {
		t.Fatal("threshold 0 must normalize to 3")
	}
}
```

- [ ] **Step 7: admin/handler.go**

`settingsResponse`: after `CodexInitialSessionMaxAgeSeconds    int  \`json:"codex_initial_session_max_age_seconds"\`` add:

```go
	CodexSessionAutoLockEnabled   bool `json:"codex_session_auto_lock_enabled"`
	CodexSessionAutoLockThreshold int  `json:"codex_session_auto_lock_threshold"`
	CodexTurnStateVaultEnabled    bool `json:"codex_turn_state_vault_enabled"`
```

`updateSettingsReq`: after `CodexInitialSessionMaxAgeSeconds    *int  \`json:"codex_initial_session_max_age_seconds"\`` add the three pointer fields (`*bool`, `*int`, `*bool`) with the same JSON tags.

Both response literals (the two places containing `CodexInitialSessionMaxAgeSeconds:    runtimeCfg.CodexInitialSessionMaxAgeSeconds,` that are responses) AND the persist literal (`&database.SystemSettings{`): add after that line

```go
		CodexSessionAutoLockEnabled:   runtimeCfg.CodexSessionAutoLockEnabled,
		CodexSessionAutoLockThreshold: runtimeCfg.CodexSessionAutoLockThreshold,
		CodexTurnStateVaultEnabled:    runtimeCfg.CodexTurnStateVaultEnabled,
```

Apply block: after the `if req.CodexInitialSessionMaxAgeSeconds != nil { ... }` block add:

```go
	if req.CodexSessionAutoLockEnabled != nil {
		runtimeCfg.CodexSessionAutoLockEnabled = *req.CodexSessionAutoLockEnabled
		log.Printf("设置已更新: codex_session_auto_lock_enabled = %t", runtimeCfg.CodexSessionAutoLockEnabled)
	}
	if req.CodexSessionAutoLockThreshold != nil {
		runtimeCfg.CodexSessionAutoLockThreshold = database.NormalizeSessionAutoLockThreshold(*req.CodexSessionAutoLockThreshold)
		log.Printf("设置已更新: codex_session_auto_lock_threshold = %d", runtimeCfg.CodexSessionAutoLockThreshold)
	}
	if req.CodexTurnStateVaultEnabled != nil {
		runtimeCfg.CodexTurnStateVaultEnabled = *req.CodexTurnStateVaultEnabled
		log.Printf("设置已更新: codex_turn_state_vault_enabled = %t", runtimeCfg.CodexTurnStateVaultEnabled)
	}
```

- [ ] **Step 8: Verify and commit**

Run: `gofmt -w database/postgres.go database/sqlite.go proxy/runtime_config.go admin/handler.go && go build ./... && go test ./database/ -run 'SessionAutoLock|SessionGuardSettings|CodexTelemetrySetting' -count=1 && go test ./admin/ -run 'TestSessionAutoLockAndVaultSettingsRoundtrip|TestSessionGuardSettingsRoundtrip|TestUpdateSettings' -count=1`
Expected: PASS. Run `gitnexus_detect_changes()`, then:

```bash
git add database/postgres.go database/sqlite.go database/session_auto_lock_settings_test.go proxy/runtime_config.go admin/handler.go admin/session_auto_lock_settings_test.go docs/superpowers/specs/2026-09-17-session-auto-lock-design.md docs/superpowers/plans/2026-09-17-session-auto-lock-and-vault.md
git commit -m "feat(session-guards): persist auto-lock and turn-state vault settings"
```

---

### Task 2: `session_auto_locks` table and CRUD

**Files:**
- Create: `database/session_auto_locks.go`
- Modify: `database/postgres.go` `New()` — after the `ensurePromptConversationLocksTable` call block (inside the same `if` or right after it, matching how `ensureProxyRiskScoringTables` is called unconditionally) add the ensure call
- Test: `database/session_auto_locks_test.go`

**Interfaces:**
- Produces: `type SessionAutoLock struct { ID int64; SessionKey, SessionIDPrefix string; APIKeyID, AccountID int64; ErrorMessage string; Threshold int; Source string; LockedAt, CreatedAt time.Time }`; `type SessionAutoLockInput struct { SessionKey, SessionIDPrefix string; APIKeyID, AccountID int64; ErrorMessage string; Threshold int }`; `(db *DB) InsertSessionAutoLock(ctx, SessionAutoLockInput) (lock *SessionAutoLock, created bool, err error)`; `(db *DB) ListSessionAutoLocks(ctx, limit int) ([]SessionAutoLock, error)`; `(db *DB) ListSessionAutoLockKeys(ctx) ([]string, error)`; `(db *DB) DeleteSessionAutoLock(ctx, id int64) (*SessionAutoLock, error)` (returns nil,nil when absent).

- [ ] **Step 1: Failing test** — create `database/session_auto_locks_test.go`:

```go
package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSessionAutoLocksCRUD(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "locks.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	in := SessionAutoLockInput{SessionKey: "sess-1::api-key:9", SessionIDPrefix: "sess-1", APIKeyID: 9, AccountID: 244, ErrorMessage: "server_is_overloaded", Threshold: 3}
	lock, created, err := db.InsertSessionAutoLock(ctx, in)
	if err != nil || !created || lock == nil || lock.ID == 0 || lock.Source != "automatic" {
		t.Fatalf("insert = %#v created=%v err=%v", lock, created, err)
	}
	again, created, err := db.InsertSessionAutoLock(ctx, in)
	if err != nil || created || again == nil || again.ID != lock.ID {
		t.Fatalf("duplicate insert must be idempotent: %#v created=%v err=%v", again, created, err)
	}
	keys, err := db.ListSessionAutoLockKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0] != in.SessionKey {
		t.Fatalf("keys = %v err=%v", keys, err)
	}
	list, err := db.ListSessionAutoLocks(ctx, 10)
	if err != nil || len(list) != 1 || list[0].AccountID != 244 || list[0].ErrorMessage != "server_is_overloaded" || list[0].Threshold != 3 {
		t.Fatalf("list = %#v err=%v", list, err)
	}
	removed, err := db.DeleteSessionAutoLock(ctx, lock.ID)
	if err != nil || removed == nil || removed.SessionKey != in.SessionKey {
		t.Fatalf("delete = %#v err=%v", removed, err)
	}
	if removed, err := db.DeleteSessionAutoLock(ctx, lock.ID); err != nil || removed != nil {
		t.Fatalf("second delete must be nil,nil: %#v %v", removed, err)
	}
	if keys, _ := db.ListSessionAutoLockKeys(ctx); len(keys) != 0 {
		t.Fatalf("keys after delete = %v", keys)
	}
	long := SessionAutoLockInput{SessionKey: "sess-2", SessionIDPrefix: "sess-2", ErrorMessage: string(make([]byte, 600)), Threshold: 0}
	lock, _, err = db.InsertSessionAutoLock(ctx, long)
	if err != nil || len(lock.ErrorMessage) != 255 || lock.Threshold != 3 {
		t.Fatalf("clamping: len=%d threshold=%d err=%v", len(lock.ErrorMessage), lock.Threshold, err)
	}
}
```

- [ ] **Step 2: Run** `go test ./database/ -run TestSessionAutoLocksCRUD -count=1` → compile error.

- [ ] **Step 3: Implement `database/session_auto_locks.go`**

```go
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// session_auto_locks：连续最终 500 达阈值后自动锁定的会话。键是网关的会话粘性键
// （会话 ID + API Key），锁没有 TTL，只有管理员手动解锁。
type SessionAutoLock struct {
	ID              int64     `json:"id"`
	SessionKey      string    `json:"session_key"`
	SessionIDPrefix string    `json:"session_id_prefix"`
	APIKeyID        int64     `json:"api_key_id"`
	AccountID       int64     `json:"account_id"`
	ErrorMessage    string    `json:"error_message"`
	Threshold       int       `json:"threshold"`
	Source          string    `json:"source"`
	LockedAt        time.Time `json:"locked_at"`
	CreatedAt       time.Time `json:"created_at"`
}

type SessionAutoLockInput struct {
	SessionKey      string
	SessionIDPrefix string
	APIKeyID        int64
	AccountID       int64
	ErrorMessage    string
	Threshold       int
}

var sessionAutoLockSchemaMu sync.Mutex

func (db *DB) ensureSessionAutoLocksTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	sessionAutoLockSchemaMu.Lock()
	defer sessionAutoLockSchemaMu.Unlock()
	idType, timeType := "BIGSERIAL PRIMARY KEY", "TIMESTAMPTZ"
	if db.isSQLite() {
		idType, timeType = "INTEGER PRIMARY KEY AUTOINCREMENT", "TIMESTAMP"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS session_auto_locks (
		id %s,
		session_key VARCHAR(255) NOT NULL UNIQUE,
		session_id_prefix VARCHAR(32) NOT NULL DEFAULT '',
		api_key_id BIGINT NOT NULL DEFAULT 0,
		account_id BIGINT NOT NULL DEFAULT 0,
		error_message VARCHAR(255) NOT NULL DEFAULT '',
		threshold INT NOT NULL DEFAULT 3,
		source VARCHAR(24) NOT NULL DEFAULT 'automatic',
		locked_at %s NOT NULL,
		created_at %s NOT NULL
	)`, idType, timeType, timeType)
	for _, statement := range []string{ddl, `CREATE INDEX IF NOT EXISTS idx_session_auto_locks_locked_at ON session_auto_locks(locked_at)`} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

const sessionAutoLockSelect = `SELECT id, session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at FROM session_auto_locks`

func scanSessionAutoLock(scanner interface{ Scan(...any) error }) (*SessionAutoLock, error) {
	var item SessionAutoLock
	if err := scanner.Scan(&item.ID, &item.SessionKey, &item.SessionIDPrefix, &item.APIKeyID, &item.AccountID, &item.ErrorMessage, &item.Threshold, &item.Source, &item.LockedAt, &item.CreatedAt); err != nil {
		return nil, err
	}
	return &item, nil
}

func clampSessionAutoLockText(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

// InsertSessionAutoLock 按 session_key 幂等：已存在时返回现有行且 created=false。
func (db *DB) InsertSessionAutoLock(ctx context.Context, input SessionAutoLockInput) (*SessionAutoLock, bool, error) {
	if db == nil {
		return nil, false, errors.New("database unavailable")
	}
	key := clampSessionAutoLockText(input.SessionKey, 255)
	if key == "" {
		return nil, false, errors.New("session key required")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, false, err
	}
	if existing, err := db.getSessionAutoLockByKey(ctx, key); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, false, nil
	}
	now := time.Now().UTC()
	query := `INSERT INTO session_auto_locks (session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, 'automatic', $7, $8)`
	if db.isSQLite() {
		query = `INSERT INTO session_auto_locks (session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at) VALUES (?, ?, ?, ?, ?, ?, 'automatic', ?, ?)`
	}
	if _, err := db.conn.ExecContext(ctx, query, key, clampSessionAutoLockText(input.SessionIDPrefix, 32), input.APIKeyID, input.AccountID, clampSessionAutoLockText(input.ErrorMessage, 255), NormalizeSessionAutoLockThreshold(input.Threshold), now, now); err != nil {
		// 并发写同一键：另一路已插入，回读即可。
		if existing, lookupErr := db.getSessionAutoLockByKey(ctx, key); lookupErr == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}
	created, err := db.getSessionAutoLockByKey(ctx, key)
	if err != nil {
		return nil, false, err
	}
	return created, true, nil
}

func (db *DB) getSessionAutoLockByKey(ctx context.Context, key string) (*SessionAutoLock, error) {
	query := sessionAutoLockSelect + ` WHERE session_key = $1`
	if db.isSQLite() {
		query = sessionAutoLockSelect + ` WHERE session_key = ?`
	}
	item, err := scanSessionAutoLock(db.conn.QueryRowContext(ctx, query, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

func (db *DB) ListSessionAutoLocks(ctx context.Context, limit int) ([]SessionAutoLock, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := sessionAutoLockSelect + ` ORDER BY locked_at DESC, id DESC LIMIT $1`
	if db.isSQLite() {
		query = sessionAutoLockSelect + ` ORDER BY locked_at DESC, id DESC LIMIT ?`
	}
	rows, err := db.conn.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionAutoLock, 0)
	for rows.Next() {
		item, err := scanSessionAutoLock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

// ListSessionAutoLockKeys 供进程启动时预热内存锁表。
func (db *DB) ListSessionAutoLockKeys(ctx context.Context) ([]string, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT session_key FROM session_auto_locks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// DeleteSessionAutoLock 返回被删除的行；不存在返回 nil, nil。
func (db *DB) DeleteSessionAutoLock(ctx context.Context, id int64) (*SessionAutoLock, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, err
	}
	query := sessionAutoLockSelect + ` WHERE id = $1`
	del := `DELETE FROM session_auto_locks WHERE id = $1`
	if db.isSQLite() {
		query = sessionAutoLockSelect + ` WHERE id = ?`
		del = `DELETE FROM session_auto_locks WHERE id = ?`
	}
	item, err := scanSessionAutoLock(db.conn.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := db.conn.ExecContext(ctx, del, id); err != nil {
		return nil, err
	}
	return item, nil
}
```

In `database/postgres.go` `New()`, directly after the block `if err := db.ensureProxyRiskScoringTables(ctx); err != nil { return nil, fmt.Errorf("创建代理风险评分表失败: %w", err) }` add:

```go
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, fmt.Errorf("创建会话自动锁表失败: %w", err)
	}
```

If SQLite `TIMESTAMP` scanning into `time.Time` fails in the test, look at how `scanPromptConversationLock` in `database/prompt_conversation_lock.go` reads its time columns on SQLite and mirror it exactly (that file is the working reference for both drivers).

- [ ] **Step 4: Verify and commit**

Run: `gofmt -w database/session_auto_locks.go && go build ./... && go test ./database/ -run 'TestSessionAutoLocksCRUD|TestSQLiteSessionAutoLockSettingsRoundtrip' -count=1`. `gitnexus_detect_changes()`, then:

```bash
git add database/session_auto_locks.go database/session_auto_locks_test.go database/postgres.go
git commit -m "feat(session-guards): session_auto_locks table and CRUD"
```

---

### Task 3: Auto-lock core (streaks, lock check, entry wiring, counters)

**Files:**
- Create: `proxy/session_auto_lock.go`
- Modify: `proxy/handler.go` (`logUsageForRequest` — add the observe call; `Responses` — set the context key and add the lock check right after `_, turnHasBinding := ...`), `proxy/responses_ws.go` (same two edits after `_, turnHasBinding := ...` at ~443), `proxy/session_guard_status.go` (add `AutoLock`), `admin/handler.go` (call `proxy.ResetSessionAutoLockStreaks()` when either auto-lock setting is in the request)
- Test: `proxy/session_auto_lock_test.go`; extend `proxy/session_guards_wiring_test.go`

**Interfaces:**
- Produces: `func (h *Handler) rememberSessionAutoLockKey(c *gin.Context, affinityKey string)`; `func (h *Handler) checkSessionAutoLock(c *gin.Context, affinityKey string) *api.APIError`; `func (h *Handler) observeSessionAutoLock(c *gin.Context, input *database.UsageLogInput)`; `func ResetSessionAutoLockStreaks()`; `func (h *Handler) UnlockSessionAutoLock(key string)`; `type SessionGuardAutoLockStatus struct { Enabled bool; Threshold int; ActiveLocks, LockedTotal, UnlockedTotal, StreakEntries uint64 }` (json tags `enabled,threshold,active_locks,locked_total,unlocked_total,streak_entries`) on `SessionGuardStatus.AutoLock`; `func sessionAutoLockSnapshot(h *Handler) SessionGuardAutoLockStatus`.

- [ ] **Step 1: Failing tests** — create `proxy/session_auto_lock_test.go`:

```go
package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newAutoLockTestHandler(t *testing.T) (*Handler, *auth.Account, *auth.Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "autolock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	official := &auth.Account{DBID: 244, AccessToken: "tok"}
	relay := &auth.Account{DBID: 300, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk-relay"}
	store.AddAccount(official)
	store.AddAccount(relay)
	resetSessionAutoLockForTest()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexSessionAutoLockEnabled = true
		s.CodexSessionAutoLockThreshold = 3
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous); resetSessionAutoLockForTest() })
	return &Handler{store: store, db: db}, official, relay
}

func autoLockTestContext(h *Handler, key string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h.rememberSessionAutoLockKey(c, key)
	return c
}

func TestSessionAutoLockLocksAfterThresholdAndRejects(t *testing.T) {
	h, official, _ := newAutoLockTestHandler(t)
	key := "thread-1::api-key:9"
	for i := 0; i < 2; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, ErrorMessage: "server_is_overloaded"})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("two failures must not lock: %v", err)
	}
	h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, ErrorMessage: "server_is_overloaded"})
	err := h.checkSessionAutoLock(autoLockTestContext(h, key), key)
	if err == nil || string(err.Code) != "session_blacklisted" {
		t.Fatalf("third failure must lock, got %v", err)
	}
	if details, _ := err.Details.(map[string]any); details == nil || details["retry"] != "stop" {
		t.Fatalf("details = %#v", err.Details)
	}
	locks, listErr := h.db.ListSessionAutoLocks(c0(), 10)
	if listErr != nil || len(locks) != 1 || locks[0].SessionKey != key || locks[0].SessionIDPrefix != "thread-1" || locks[0].APIKeyID != 9 || locks[0].AccountID != 244 {
		t.Fatalf("persisted lock = %#v err=%v", locks, listErr)
	}
	status := sessionAutoLockSnapshot(h)
	if status.ActiveLocks != 1 || status.LockedTotal != 1 {
		t.Fatalf("status = %+v", status)
	}
	h.UnlockSessionAutoLock(key)
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("unlocked session must pass: %v", err)
	}
	if status := sessionAutoLockSnapshot(h); status.ActiveLocks != 0 || status.UnlockedTotal != 1 {
		t.Fatalf("status after unlock = %+v", status)
	}
}

func TestSessionAutoLockResetsOnSuccessRetryInternalRelayAndDisabled(t *testing.T) {
	h, official, relay := newAutoLockTestHandler(t)
	key := "thread-2::api-key:9"
	fail := &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID}
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 200, AccountID: official.DBID})
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("a success must reset the streak: %v", err)
	}
	for i := 0; i < 5; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, IsRetryAttempt: true})
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, InternalReason: "title"})
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: relay.DBID})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("retry/internal/relay results must not count: %v", err)
	}
	if got := sessionAutoLockSnapshot(h).StreakEntries; got != 1 {
		t.Fatalf("streak entries = %d, want 1 (the two real failures)", got)
	}
	ResetSessionAutoLockStreaks()
	if got := sessionAutoLockSnapshot(h).StreakEntries; got != 0 {
		t.Fatalf("reset must clear streaks, got %d", got)
	}
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionAutoLockEnabled = false; return s })
	for i := 0; i < 5; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("disabled guard must never lock: %v", err)
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, ""), ""); err != nil {
		t.Fatalf("empty key must pass: %v", err)
	}
}

func TestSessionAutoLockWarmsExistingLocksFromDatabase(t *testing.T) {
	h, _, _ := newAutoLockTestHandler(t)
	if _, _, err := h.db.InsertSessionAutoLock(c0(), database.SessionAutoLockInput{SessionKey: "old::api-key:1", SessionIDPrefix: "old", Threshold: 3}); err != nil {
		t.Fatal(err)
	}
	resetSessionAutoLockForTest()
	if err := h.checkSessionAutoLock(autoLockTestContext(h, "old::api-key:1"), "old::api-key:1"); err == nil {
		t.Fatal("lock persisted in the database must be enforced after a restart")
	}
}
```

Add at the bottom of the same test file a tiny helper: `func c0() context.Context { return context.Background() }` (and import `context`).

- [ ] **Step 2: Run** `go test ./proxy/ -run 'TestSessionAutoLock' -count=1` → compile error.

- [ ] **Step 3: Implement `proxy/session_auto_lock.go`**

```go
package proxy

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 连续 500 自动锁定：以会话粘性键为身份，只统计官方 Codex 账号上最终完成的 500
// （中转账号、内部重试、内部子请求不计），达阈值写锁（DB + 内存），入口按键拒绝。
// 连击只在内存里，重启和保存设置都清零；锁没有 TTL，只有管理员解锁。

const (
	sessionAutoLockKeyContextKey = "codex2api.session_auto_lock.key"
	sessionAutoLockStreakCap     = 50000
	sessionAutoLockSource        = "automatic"
)

type sessionAutoLockStreak struct {
	count     int
	updatedAt time.Time
}

type sessionAutoLockState struct {
	mu            sync.Mutex
	streaks       map[string]*sessionAutoLockStreak
	locked        map[string]struct{}
	loaded        bool
	lockedTotal   atomic.Uint64
	unlockedTotal atomic.Uint64
}

var sessionAutoLock = newSessionAutoLockState()

func newSessionAutoLockState() *sessionAutoLockState {
	return &sessionAutoLockState{streaks: make(map[string]*sessionAutoLockStreak), locked: make(map[string]struct{})}
}

func resetSessionAutoLockForTest() {
	fresh := newSessionAutoLockState()
	sessionAutoLock.mu.Lock()
	sessionAutoLock.streaks, sessionAutoLock.locked, sessionAutoLock.loaded = fresh.streaks, fresh.locked, false
	sessionAutoLock.mu.Unlock()
	sessionAutoLock.lockedTotal.Store(0)
	sessionAutoLock.unlockedTotal.Store(0)
}

// ResetSessionAutoLockStreaks 保存设置时调用：未触发的连击全部清零，已有锁不动。
func ResetSessionAutoLockStreaks() {
	sessionAutoLock.mu.Lock()
	sessionAutoLock.streaks = make(map[string]*sessionAutoLockStreak)
	sessionAutoLock.mu.Unlock()
}

func (h *Handler) rememberSessionAutoLockKey(c *gin.Context, affinityKey string) {
	if c == nil {
		return
	}
	if key := strings.TrimSpace(affinityKey); key != "" {
		c.Set(sessionAutoLockKeyContextKey, key)
	}
}

func sessionAutoLockKeyFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, ok := c.Get(sessionAutoLockKeyContextKey); ok {
		if key, ok := value.(string); ok {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

// ensureLoadedLocked 首次使用时从 DB 预热锁表；失败只打日志（下次再试）。
func (h *Handler) ensureSessionAutoLocksLoadedLocked() {
	if sessionAutoLock.loaded || h == nil || h.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	keys, err := h.db.ListSessionAutoLockKeys(ctx)
	if err != nil {
		log.Printf("[SESSION-AUTO-LOCK] 预热锁表失败: %v", err)
		return
	}
	for _, key := range keys {
		sessionAutoLock.locked[key] = struct{}{}
	}
	sessionAutoLock.loaded = true
}

func sessionAutoLockError() *api.APIError {
	return api.NewAPIErrorWithDetails(api.ErrorCode("session_blacklisted"), "该会话因连续上游错误已被锁定，请新建对话；如需恢复请管理员解锁。", api.ErrorTypeInvalidRequest, map[string]any{"retry": "stop"})
}

// checkSessionAutoLock 入口检查：命中锁即拒绝，与账号类型无关（计数阶段已排除中转）。
func (h *Handler) checkSessionAutoLock(c *gin.Context, affinityKey string) *api.APIError {
	key := strings.TrimSpace(affinityKey)
	if key == "" {
		return nil
	}
	sessionAutoLock.mu.Lock()
	h.ensureSessionAutoLocksLoadedLocked()
	_, locked := sessionAutoLock.locked[key]
	sessionAutoLock.mu.Unlock()
	if !locked {
		return nil
	}
	return sessionAutoLockError()
}

func sessionIDPrefixFromAffinityKey(key string) string {
	session := key
	if idx := strings.Index(key, "::api-key:"); idx >= 0 {
		session = key[:idx]
	}
	if len(session) > 12 {
		session = session[:12]
	}
	return session
}

func apiKeyIDFromAffinityKey(key string) int64 {
	idx := strings.Index(key, "::api-key:")
	if idx < 0 {
		return 0
	}
	var id int64
	for _, ch := range key[idx+len("::api-key:"):] {
		if ch < '0' || ch > '9' {
			break
		}
		id = id*10 + int64(ch-'0')
	}
	return id
}

// observeSessionAutoLock 在 logUsageForRequest 里调用：按最终结果维护连击并落锁。
func (h *Handler) observeSessionAutoLock(c *gin.Context, input *database.UsageLogInput) {
	if h == nil || input == nil {
		return
	}
	settings := CurrentRuntimeSettings()
	if !settings.CodexSessionAutoLockEnabled {
		return
	}
	key := sessionAutoLockKeyFromContext(c)
	if key == "" || input.IsRetryAttempt || strings.TrimSpace(input.InternalReason) != "" || input.AccountID <= 0 {
		return
	}
	if h.store == nil {
		return
	}
	account := h.store.FindByID(input.AccountID)
	if account == nil || account.IsRelayStyle() {
		return
	}
	threshold := database.NormalizeSessionAutoLockThreshold(settings.CodexSessionAutoLockThreshold)
	now := time.Now()
	sessionAutoLock.mu.Lock()
	if input.StatusCode != 500 {
		delete(sessionAutoLock.streaks, key)
		sessionAutoLock.mu.Unlock()
		return
	}
	streak := sessionAutoLock.streaks[key]
	if streak == nil {
		if len(sessionAutoLock.streaks) >= sessionAutoLockStreakCap {
			evictOldestSessionAutoLockStreaksLocked(len(sessionAutoLock.streaks) / 10)
		}
		streak = &sessionAutoLockStreak{}
		sessionAutoLock.streaks[key] = streak
	}
	streak.count++
	streak.updatedAt = now
	if streak.count < threshold {
		sessionAutoLock.mu.Unlock()
		return
	}
	delete(sessionAutoLock.streaks, key)
	_, already := sessionAutoLock.locked[key]
	sessionAutoLock.locked[key] = struct{}{}
	sessionAutoLock.mu.Unlock()
	if already {
		return
	}
	sessionAutoLock.lockedTotal.Add(1)
	log.Printf("[SESSION-AUTO-LOCK] locked session=%s account=%d threshold=%d error=%q", sessionIDPrefixFromAffinityKey(key), input.AccountID, threshold, strings.TrimSpace(input.ErrorMessage))
	if h.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := h.db.InsertSessionAutoLock(ctx, database.SessionAutoLockInput{
		SessionKey: key, SessionIDPrefix: sessionIDPrefixFromAffinityKey(key), APIKeyID: apiKeyIDFromAffinityKey(key),
		AccountID: input.AccountID, ErrorMessage: input.ErrorMessage, Threshold: threshold,
	}); err != nil {
		log.Printf("[SESSION-AUTO-LOCK] 写锁失败（内存锁仍生效）: %v", err)
	}
}

func evictOldestSessionAutoLockStreaksLocked(n int) {
	type entry struct {
		key string
		at  time.Time
	}
	entries := make([]entry, 0, len(sessionAutoLock.streaks))
	for key, streak := range sessionAutoLock.streaks {
		entries = append(entries, entry{key, streak.updatedAt})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	if n < 1 {
		n = 1
	}
	for i := 0; i < n && i < len(entries); i++ {
		delete(sessionAutoLock.streaks, entries[i].key)
	}
}

// UnlockSessionAutoLock 管理员解锁后从内存移除（DB 行由 admin 层删除）。
func (h *Handler) UnlockSessionAutoLock(key string) {
	key = strings.TrimSpace(key)
	sessionAutoLock.mu.Lock()
	_, existed := sessionAutoLock.locked[key]
	delete(sessionAutoLock.locked, key)
	delete(sessionAutoLock.streaks, key)
	sessionAutoLock.mu.Unlock()
	if existed {
		sessionAutoLock.unlockedTotal.Add(1)
	}
}

type SessionGuardAutoLockStatus struct {
	Enabled       bool   `json:"enabled"`
	Threshold     int    `json:"threshold"`
	ActiveLocks   uint64 `json:"active_locks"`
	LockedTotal   uint64 `json:"locked_total"`
	UnlockedTotal uint64 `json:"unlocked_total"`
	StreakEntries uint64 `json:"streak_entries"`
}

func sessionAutoLockSnapshot(h *Handler) SessionGuardAutoLockStatus {
	settings := CurrentRuntimeSettings()
	sessionAutoLock.mu.Lock()
	if h != nil {
		h.ensureSessionAutoLocksLoadedLocked()
	}
	active, streaks := uint64(len(sessionAutoLock.locked)), uint64(len(sessionAutoLock.streaks))
	sessionAutoLock.mu.Unlock()
	return SessionGuardAutoLockStatus{
		Enabled: settings.CodexSessionAutoLockEnabled, Threshold: database.NormalizeSessionAutoLockThreshold(settings.CodexSessionAutoLockThreshold),
		ActiveLocks: active, LockedTotal: sessionAutoLock.lockedTotal.Load(), UnlockedTotal: sessionAutoLock.unlockedTotal.Load(), StreakEntries: streaks,
	}
}
```

- [ ] **Step 4: Wire it**

`proxy/handler.go` `logUsageForRequest`: directly after `populateUltraUsageMetaFromRequest(c, input)` add `h.observeSessionAutoLock(c, input)`.

`proxy/handler.go` `Responses`: directly after the line `_, turnHasBinding := h.store.SessionAffinityAccountID(affinityKey)` (before the admission `if failure := ...` block) add:

```go
	h.rememberSessionAutoLockKey(c, affinityKey)
	if failure := h.checkSessionAutoLock(c, affinityKey); failure != nil {
		api.SendErrorWithStatus(c, failure, http.StatusBadRequest)
		return
	}
```

`proxy/responses_ws.go` directly after its `_, turnHasBinding := h.store.SessionAffinityAccountID(affinityKey)` add:

```go
	h.rememberSessionAutoLockKey(c, affinityKey)
	if failure := h.checkSessionAutoLock(c, affinityKey); failure != nil {
		_ = writeResponsesWSError(conn, failure)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, failure.Message, failure)
	}
```

`proxy/session_guard_status.go`: add field `AutoLock SessionGuardAutoLockStatus \`json:"auto_lock"\`` to `SessionGuardStatus` and, in `SessionGuardStatusSnapshot`, set `status.AutoLock = sessionAutoLockSnapshot(nil)`; then change the signature to `SessionGuardStatusSnapshot(store *auth.Store, handler *Handler)` — NO: keep the signature; instead add a new exported `func SessionGuardStatusSnapshotForHandler(h *Handler) SessionGuardStatus` that calls `SessionGuardStatusSnapshot(h.store)` and then sets `status.AutoLock = sessionAutoLockSnapshot(h)`; the admin runtime status (Task 4) switches to it via the `authCacheProxy *proxy.Handler` field it already holds (fall back to `SessionGuardStatusSnapshot(h.store)` when that field is nil).

`admin/handler.go` apply block: directly after the three new `if req.Codex...` blocks from Task 1 add:

```go
	if req.CodexSessionAutoLockEnabled != nil || req.CodexSessionAutoLockThreshold != nil {
		proxy.ResetSessionAutoLockStreaks()
	}
```

`proxy/session_guards_wiring_test.go`: add inside `TestSessionGuardWiringPresent`:

```go
	for name, src := range map[string][]byte{"handler.go": handler, "responses_ws.go": ws} {
		if got := regexp.MustCompile(`h\.checkSessionAutoLock\(c, affinityKey\)`).FindAll(src, -1); len(got) != 1 {
			t.Fatalf("%s auto-lock check sites = %d, want 1", name, len(got))
		}
		if got := regexp.MustCompile(`h\.rememberSessionAutoLockKey\(c, affinityKey\)`).FindAll(src, -1); len(got) != 1 {
			t.Fatalf("%s auto-lock key sites = %d, want 1", name, len(got))
		}
	}
	if !regexp.MustCompile(`h\.observeSessionAutoLock\(c, input\)`).Match(handler) {
		t.Fatal("logUsageForRequest must feed the auto-lock streaks")
	}
```

- [ ] **Step 5: Verify and commit**

Run: `gofmt -w proxy/session_auto_lock.go proxy/handler.go proxy/responses_ws.go proxy/session_guard_status.go admin/handler.go && go build ./... && go test ./proxy/ -run 'TestSessionAutoLock|TestSessionGuardWiringPresent' -count=1 && go test ./proxy/ -count=1`. `gitnexus_impact` for `logUsageForRequest` (expect HIGH — additive) and `gitnexus_detect_changes()`. Commit:

```bash
git add proxy/session_auto_lock.go proxy/session_auto_lock_test.go proxy/handler.go proxy/responses_ws.go proxy/session_guard_status.go proxy/session_guards_wiring_test.go admin/handler.go
git commit -m "feat(session-guards): auto-lock sessions after consecutive final 500s"
```

---

### Task 4: Admin API for session locks

**Files:**
- Create: `admin/session_locks.go`
- Modify: `admin/handler.go` routes (after `api.GET("/runtime-status", h.GetRuntimeStatus)` add `api.GET("/session-locks", h.ListSessionLocks)` and `api.DELETE("/session-locks/:id", h.DeleteSessionLock)`); `admin/runtime_status.go` (`SessionGuards: proxy.SessionGuardStatusSnapshotForHandler(h.authCacheProxy)` when `h.authCacheProxy != nil`, else the existing call)
- Test: `admin/session_locks_test.go`

**Interfaces:**
- Produces: `GET /api/admin/session-locks?limit=` → `{"locks":[sessionLockResponse],"total":N}` with fields `id, session_id_prefix, api_key_id, account_id, account_name, error_message, threshold, source, locked_at`; `DELETE /api/admin/session-locks/:id` → 200 `{"message":"unlocked"}` or 404.

- [ ] **Step 1: Failing test** — `admin/session_locks_test.go`:

```go
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestSessionLocksListAndUnlock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 244, Email: "acct@example.com", AccessToken: "tok"})
	h := &Handler{store: store, db: db}
	lock, _, err := db.InsertSessionAutoLock(context.Background(), database.SessionAutoLockInput{SessionKey: "thread-9::api-key:2", SessionIDPrefix: "thread-9", APIKeyID: 2, AccountID: 244, ErrorMessage: "server_is_overloaded", Threshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/session-locks?limit=10", nil)
	h.ListSessionLocks(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Locks []map[string]any `json:"locks"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Total != 1 || len(out.Locks) != 1 {
		t.Fatalf("list body = %s err=%v", rec.Body.String(), err)
	}
	if out.Locks[0]["session_id_prefix"] != "thread-9" || out.Locks[0]["account_name"] != "acct@example.com" || out.Locks[0]["api_key_id"] != float64(2) {
		t.Fatalf("row = %v", out.Locks[0])
	}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/session-locks/1", nil)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	h.DeleteSessionLock(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if removed, err := db.DeleteSessionAutoLock(context.Background(), lock.ID); err != nil || removed != nil {
		t.Fatalf("row must be gone: %#v %v", removed, err)
	}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/session-locks/1", nil)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	h.DeleteSessionLock(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}
```

- [ ] **Step 2: Run** `go test ./admin/ -run TestSessionLocksListAndUnlock -count=1` → compile error.

- [ ] **Step 3: Implement `admin/session_locks.go`**

```go
package admin

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type sessionLockResponse struct {
	ID              int64  `json:"id"`
	SessionIDPrefix string `json:"session_id_prefix"`
	APIKeyID        int64  `json:"api_key_id"`
	AccountID       int64  `json:"account_id"`
	AccountName     string `json:"account_name"`
	ErrorMessage    string `json:"error_message"`
	Threshold       int    `json:"threshold"`
	Source          string `json:"source"`
	LockedAt        string `json:"locked_at"`
}

// ListSessionLocks 列出自动锁定的会话（最新在前）。
func (h *Handler) ListSessionLocks(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	limit, _ := strconv.Atoi(c.Query("limit"))
	locks, err := h.db.ListSessionAutoLocks(ctx, limit)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	out := make([]sessionLockResponse, 0, len(locks))
	for _, lock := range locks {
		name := ""
		if h.store != nil && lock.AccountID > 0 {
			if account := h.store.FindByID(lock.AccountID); account != nil {
				name = account.Email
			}
		}
		out = append(out, sessionLockResponse{
			ID: lock.ID, SessionIDPrefix: lock.SessionIDPrefix, APIKeyID: lock.APIKeyID, AccountID: lock.AccountID, AccountName: name,
			ErrorMessage: lock.ErrorMessage, Threshold: lock.Threshold, Source: lock.Source, LockedAt: lock.LockedAt.UTC().Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"locks": out, "total": len(out)})
}

// DeleteSessionLock 管理员解锁：删 DB 行并从进程内存移除。
func (h *Handler) DeleteSessionLock(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的锁 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	removed, err := h.db.DeleteSessionAutoLock(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if removed == nil {
		writeError(c, http.StatusNotFound, "锁不存在或已解除")
		return
	}
	if h.authCacheProxy != nil {
		h.authCacheProxy.UnlockSessionAutoLock(removed.SessionKey)
	}
	c.JSON(http.StatusOK, gin.H{"message": "unlocked"})
}
```

If `writeInternalError` / `writeError` have different names in `admin/`, use the helpers `ListAccountGroups` uses. If `Account.Email` is not the display field used elsewhere in admin (check `admin/account_response_builder.go` for how the name/email is derived), use that.

Routes + runtime status edits as listed in Files.

- [ ] **Step 4: Verify and commit**

Run: `gofmt -w admin/session_locks.go admin/handler.go admin/runtime_status.go && go build ./... && go test ./admin/ -run 'TestSessionLocksListAndUnlock|TestRuntimeStatus' -count=1`. `gitnexus_detect_changes()`. Commit:

```bash
git add admin/session_locks.go admin/session_locks_test.go admin/handler.go admin/runtime_status.go
git commit -m "feat(session-guards): admin API to list and unlock auto-locked sessions"
```

---

### Task 5: Initial-session admission moves behind account selection (relay exemption)

**Files:**
- Modify: `proxy/initial_session_admission.go` (add memoized `enforceInitialSessionAdmission`), `proxy/handler.go` (remove the pre-selection block after `turnHasBinding`; add the post-selection check in the official path), `proxy/responses_ws.go` (same), `proxy/session_guards_wiring_test.go` (update regexes)
- Test: extend `proxy/initial_session_admission_test.go`

**Interfaces:**
- Produces: `func (h *Handler) enforceInitialSessionAdmission(c *gin.Context, account *auth.Account, headers http.Header, body []byte, identity requestSessionIdentity, hasBinding bool, received time.Time) *api.APIError` — returns nil immediately when `account == nil || account.IsRelayStyle()`; evaluates `checkInitialSessionAdmission` at most once per gin context (memo key `codex2api.initial_session.verdict`), returning the memoized error on later attempts.

- [ ] **Step 1: Failing test** — append to `proxy/initial_session_admission_test.go`:

```go
func TestEnforceInitialSessionAdmissionSkipsRelayAndMemoizes(t *testing.T) {
	resetSessionGuardStatsForTest()
	setInitialAdmission(t, true, 180)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	h := &Handler{store: store}
	relay := &auth.Account{DBID: 300, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk"}
	official := &auth.Account{DBID: 244, AccessToken: "tok"}
	now := time.Now()
	old := v7At(t, now.Add(-time.Hour))
	body := []byte(`{"model":"gpt-5.5","input":[]}`)
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		return c
	}
	if err := h.enforceInitialSessionAdmission(newCtx(), relay, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now); err != nil {
		t.Fatalf("relay-style account must be exempt: %v", err)
	}
	if _, since := sessionGuardInitialSnapshot(time.Now()); since.Samples != 0 {
		t.Fatalf("relay exemption must not even sample: %+v", since)
	}
	c := newCtx()
	first := h.enforceInitialSessionAdmission(c, official, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now)
	if first == nil {
		t.Fatal("old unbound session on an official account must be rejected")
	}
	second := h.enforceInitialSessionAdmission(c, official, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now)
	if second == nil || second != first {
		t.Fatal("second attempt on the same request must return the memoized verdict")
	}
	if _, since := sessionGuardInitialSnapshot(time.Now()); since.Samples != 1 {
		t.Fatalf("memoized verdict must be sampled once: %+v", since)
	}
}
```

(`gin`, `httptest`, `net/http` imports may need adding to that test file.)

- [ ] **Step 2: Run** → compile error (`enforceInitialSessionAdmission undefined`).

- [ ] **Step 3: Implement** — append to `proxy/initial_session_admission.go`:

```go
const initialSessionVerdictContextKey = "codex2api.initial_session.verdict"

// enforceInitialSessionAdmission 选号之后调用：中转账号直接放行；同一请求只判定一次，
// failover 换号重试沿用首次结论（年龄不会因为换号而变）。
func (h *Handler) enforceInitialSessionAdmission(c *gin.Context, account *auth.Account, headers http.Header, body []byte, identity requestSessionIdentity, hasBinding bool, received time.Time) *api.APIError {
	if account == nil || account.IsRelayStyle() {
		return nil
	}
	if c != nil {
		if cached, ok := c.Get(initialSessionVerdictContextKey); ok {
			if failure, _ := cached.(*api.APIError); failure != nil {
				return failure
			}
			return nil
		}
	}
	failure := h.checkInitialSessionAdmission(headers, body, identity, hasBinding, received)
	if c != nil {
		c.Set(initialSessionVerdictContextKey, failure)
	}
	return failure
}
```

(`gin` and `auth` imports.) Note: storing a typed nil `*api.APIError` in the context is fine — the read side type-asserts and treats nil as pass.

`proxy/handler.go`: delete the pre-selection block (the `if failure := h.checkInitialSessionAdmission(...) { ...comment...; api.SendErrorWithStatus(c, failure, http.StatusBadRequest); return }` right after the auto-lock check). In the attempt loop's official path, directly after the line `upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)` (the first statement after the relay branch's closing `}`), add:

```go
		if failure := h.enforceInitialSessionAdmission(c, account, c.Request.Header, rawBody, sessionIdentity, turnHasBinding, handlerStart); failure != nil {
			h.store.Release(account)
			// codex_session_identity_unavailable 不在 api.HTTPStatusCode 的显式分支里，显式给 400。
			api.SendErrorWithStatus(c, failure, http.StatusBadRequest)
			return
		}
```

Check whether, at that point of the loop, an API-key scope concurrency slot or similar was already acquired for `account` (grep the ~40 lines above for `AcquireAPIKeyScopeConcurrency` / `bindContinuousRetrySessionAffinityWithGuard`); if the nearest existing early-return there does more than `h.store.Release(account)`, mirror it exactly and say so in the report.

`proxy/responses_ws.go`: delete the pre-selection admission block; directly after `downstreamHeaders := c.Request.Header.Clone()` inside the attempt loop (≈ line 700), add:

```go
		if failure := h.enforceInitialSessionAdmission(c, account, c.Request.Header, rawBody, sessionIdentity, turnHasBinding, time.Now()); failure != nil {
			h.store.Release(account)
			_ = writeResponsesWSError(conn, failure)
			return newResponsesWSCloseError(websocket.ClosePolicyViolation, failure.Message, failure)
		}
```

Mirror the cleanup of the `bindContinuousRetrySessionAffinityWithGuard` failure return a few lines above (it does `h.store.Release(account)`); if `h.AcquireAPIKeyScopeConcurrency(c, account)` needs a paired release there, add it exactly as other early returns in that loop do.

`proxy/session_guards_wiring_test.go`: replace the two `checkInitialSessionAdmission\(c\.Request\.Header, rawBody, sessionIdentity, turnHasBinding,` assertions with `h\.enforceInitialSessionAdmission\(c, account, c\.Request\.Header, rawBody, sessionIdentity, turnHasBinding,` (exactly 1 per file), and add `if regexp.MustCompile(`h\.checkInitialSessionAdmission\(`).Match(handler) || regexp.MustCompile(`h\.checkInitialSessionAdmission\(`).Match(ws) { t.Fatal("pre-selection admission call must be gone") }`. Keep the `SendErrorWithStatus(c, failure, http.StatusBadRequest)` assertion.

- [ ] **Step 4: Verify and commit**

Run: `gofmt -w proxy/initial_session_admission.go proxy/handler.go proxy/responses_ws.go proxy/session_guards_wiring_test.go && go build ./... && go test ./proxy/ -count=1`. `gitnexus_impact` for `Responses` and `forwardResponsesWebSocketTurn`; `gitnexus_detect_changes()`. Commit:

```bash
git add proxy/initial_session_admission.go proxy/initial_session_admission_test.go proxy/handler.go proxy/responses_ws.go proxy/session_guards_wiring_test.go
git commit -m "fix(session-guards): evaluate initial-session admission after selection and exempt relay accounts"
```

---

### Task 6: Turn-state vault (substitute tokens)

**Files:**
- Create: `proxy/turn_state_vault.go`
- Modify: `proxy/codex_turn_state.go` (`relayCodexTurnStateResponseHeader`, `commitResponsesStreamAttempt` emit substitutes), `proxy/session_guards.go` (`applyCodexTurnStateEchoPolicy` resolves substitutes; vault counters in snapshot types), `proxy/handler.go` (event rewrite after `sanitizeCapacityShedEventForClient(eventType, data)` inside `writeDeferredSSEData(...)`), `proxy/responses_ws.go` (after `clientData = sanitizeCapacityShedEventForClient(eventType, clientData)`), `proxy/session_guard_status.go` (vault counters)
- Test: `proxy/turn_state_vault_test.go`; extend `proxy/session_guards_wiring_test.go`

**Interfaces:**
- Produces: `func issueCodexTurnStateSubstitute(affinityKey string, account *auth.Account, real string) string` (empty when vault disabled/inputs empty); `func resolveCodexTurnStateSubstitute(affinityKey string, account *auth.Account, inbound string) (real string, class turnStateEchoClass)`; `func (h *Handler) vaultCodexTurnStateEvent(affinityKey string, account *auth.Account, eventType string, data []byte) []byte`; `type SessionGuardVaultCounters struct { Issued, Restored, ForeignStripped uint64 }` (json `issued,restored,foreign_stripped`) exposed as `SessionGuardTurnStateStatus.Vault`; `func resetTurnStateVaultForTest()`.

- [ ] **Step 1: Failing tests** — `proxy/turn_state_vault_test.go`:

```go
package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func setVault(t *testing.T, on bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexTurnStateVaultEnabled = on; s.CodexTurnStateStrict = false; return s })
	t.Cleanup(func() { ApplyRuntimeSettings(previous); resetTurnStateVaultForTest() })
	resetTurnStateVaultForTest()
}

func TestTurnStateVaultIssueAndResolve(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	key := "vault-1::api-key:9"
	sub := issueCodexTurnStateSubstitute(key, minter, "real-blob")
	if !strings.HasPrefix(sub, "c2a-ts-v1.") || len(sub) != len("c2a-ts-v1.")+32 || sub == "real-blob" {
		t.Fatalf("substitute = %q", sub)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, sub); real != "real-blob" || class != turnStateEchoSame {
		t.Fatalf("same-account resolve = %q %s", real, class)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, other, sub); real != "" || class != turnStateEchoCross {
		t.Fatalf("cross-account resolve = %q %s", real, class)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, "real-blob"); real != "" || class != turnStateEchoUnknown {
		t.Fatalf("a real token echoed back must be treated as foreign: %q %s", real, class)
	}
	second := issueCodexTurnStateSubstitute(key, minter, "real-blob-2")
	if _, class := resolveCodexTurnStateSubstitute(key, minter, sub); class != turnStateEchoUnknown {
		t.Fatal("a previous turn's substitute must not resolve after a new mint")
	}
	if real, _ := resolveCodexTurnStateSubstitute(key, minter, second); real != "real-blob-2" {
		t.Fatal("latest mint must resolve")
	}
	setVault(t, false)
	if sub := issueCodexTurnStateSubstitute(key, minter, "real"); sub != "" {
		t.Fatalf("disabled vault must not issue: %q", sub)
	}
}

func TestTurnStateVaultExpires(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	key := "vault-exp::api-key:9"
	sub := issueCodexTurnStateSubstitute(key, minter, "real")
	turnStateVault.Range(func(k, v any) bool {
		if k == key {
			entry := v.(*turnStateVaultEntry)
			entry.expiresAt = time.Now().Add(-time.Second)
		}
		return true
	})
	if _, class := resolveCodexTurnStateSubstitute(key, minter, sub); class != turnStateEchoUnknown {
		t.Fatalf("expired entry must be unknown, got %s", class)
	}
}

func TestVaultCodexTurnStateEventRewritesMetadataHeaders(t *testing.T) {
	setVault(t, true)
	h := &Handler{}
	minter := &auth.Account{DBID: 101}
	key := "vault-ev::api-key:9"
	for _, eventType := range []string{"response.metadata", "codex.response.metadata"} {
		data := []byte(`{"type":"` + eventType + `","headers":{"X-Codex-Turn-State":"real-blob","openai-model":"gpt-5.5"},"metadata":{"k":"v"}}`)
		out := h.vaultCodexTurnStateEvent(key, minter, eventType, data)
		got := gjson.GetBytes(out, "headers.X-Codex-Turn-State").String()
		if got == "real-blob" || !strings.HasPrefix(got, "c2a-ts-v1.") {
			t.Fatalf("%s: token not substituted: %s", eventType, out)
		}
		if gjson.GetBytes(out, "headers.openai-model").String() != "gpt-5.5" || gjson.GetBytes(out, "metadata.k").String() != "v" {
			t.Fatalf("%s: sibling fields damaged: %s", eventType, out)
		}
		if real, class := resolveCodexTurnStateSubstitute(key, minter, got); real != "real-blob" || class != turnStateEchoSame {
			t.Fatalf("%s: substitute must resolve: %q %s", eventType, real, class)
		}
	}
	untouched := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if out := h.vaultCodexTurnStateEvent(key, minter, "response.output_text.delta", untouched); string(out) != string(untouched) {
		t.Fatal("non-metadata events must pass through unchanged")
	}
	noToken := []byte(`{"type":"response.metadata","headers":{"openai-model":"gpt-5.5"}}`)
	if out := h.vaultCodexTurnStateEvent(key, minter, "response.metadata", noToken); string(out) != string(noToken) {
		t.Fatal("metadata without a token must pass through unchanged")
	}
}

func TestApplyCodexTurnStateEchoPolicyRestoresSubstitute(t *testing.T) {
	setVault(t, true)
	resetSessionGuardStatsForTest()
	minter := &auth.Account{DBID: 101, AccessToken: "tok"}
	other := &auth.Account{DBID: 202, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, minter, other)
	key := "vault-policy::api-key:9"
	sub := issueCodexTurnStateSubstitute(key, minter, "real-blob")
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, sub)
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"`+sub+`"}}`))
	if class != turnStateEchoSame || stripped || headers.Get(codexTurnStateHeader) != "real-blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "real-blob" {
		t.Fatalf("substitute must be restored on both carriers: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}
	headers.Set(codexTurnStateHeader, sub)
	if _, class, stripped := h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{}`)); class != turnStateEchoCross || !stripped || headers.Get(codexTurnStateHeader) != "" {
		t.Fatalf("cross-account substitute must be stripped: %s %v", class, stripped)
	}
	headers.Set(codexTurnStateHeader, "foreign-real-token")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"foreign-real-token"}}`))
	if class != turnStateEchoUnknown || !stripped || headers.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("foreign token must always be stripped under the vault even with strict off: %s %v", class, stripped)
	}
	_, accounts := sessionGuardTurnStateSnapshot()
	if len(accounts) == 0 {
		t.Fatal("observations must still be counted")
	}
	if v := turnStateVaultCountersSnapshot(); v.Issued != 1 || v.Restored != 1 || v.ForeignStripped != 1 {
		t.Fatalf("vault counters = %+v", v)
	}
}

func TestRelayCodexTurnStateResponseHeaderEmitsSubstitute(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	key := "vault-relay::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	c, rec := newTurnStateTestContext(t)
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "real-blob")
	relayCodexTurnStateResponseHeader(c, key, minter, upstream)
	got := c.Writer.Header().Get(codexTurnStateHeader)
	_ = rec
	if got == "" || got == "real-blob" || !strings.HasPrefix(got, "c2a-ts-v1.") {
		t.Fatalf("client must receive a substitute, got %q", got)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, got); real != "real-blob" || class != turnStateEchoSame {
		t.Fatalf("relayed substitute must resolve: %q %s", real, class)
	}
}
```

- [ ] **Step 2: Run** `go test ./proxy/ -run 'TurnStateVault|VaultCodexTurnState|RestoresSubstitute|EmitsSubstitute' -count=1` → compile error.

- [ ] **Step 3: Implement `proxy/turn_state_vault.go`**

```go
package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// turn-state 托管：真实的 X-Codex-Turn-State 只留在网关。上游铸造时按亲和键存下
// 真实值和铸造账号，给客户端一个随机替身；客户端回带替身，出站前换回真实值。
// 客户端拿不到真实 token，就无法带去别的账号/网关再带回来污染铸造账号。
// 官方契约：token 每轮一枚、本轮内原样回带（openai/codex codex-rs client.rs）。

const (
	codexTurnStateSubstitutePrefix = "c2a-ts-v1."
	turnStateVaultTTL              = time.Hour
	turnStateVaultSweepEvery       = 256
)

type turnStateVaultEntry struct {
	real       string
	substitute string
	accountID  int64
	expiresAt  time.Time
}

type SessionGuardVaultCounters struct {
	Issued          uint64 `json:"issued"`
	Restored        uint64 `json:"restored"`
	ForeignStripped uint64 `json:"foreign_stripped"`
}

var (
	turnStateVault        sync.Map // affinityKey -> *turnStateVaultEntry
	turnStateVaultWrites  atomic.Uint64
	turnStateVaultIssued  atomic.Uint64
	turnStateVaultRestore atomic.Uint64
	turnStateVaultForeign atomic.Uint64
)

func resetTurnStateVaultForTest() {
	turnStateVault.Range(func(k, _ any) bool { turnStateVault.Delete(k); return true })
	turnStateVaultIssued.Store(0)
	turnStateVaultRestore.Store(0)
	turnStateVaultForeign.Store(0)
}

func turnStateVaultCountersSnapshot() SessionGuardVaultCounters {
	return SessionGuardVaultCounters{Issued: turnStateVaultIssued.Load(), Restored: turnStateVaultRestore.Load(), ForeignStripped: turnStateVaultForeign.Load()}
}

func turnStateVaultEnabled() bool { return CurrentRuntimeSettings().CodexTurnStateVaultEnabled }

func newCodexTurnStateSubstitute() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	return codexTurnStateSubstitutePrefix + hex.EncodeToString(raw[:])
}

// issueCodexTurnStateSubstitute 记录真实 token 并返回替身；托管关闭或输入为空返回 ""。
func issueCodexTurnStateSubstitute(affinityKey string, account *auth.Account, real string) string {
	affinityKey, real = strings.TrimSpace(affinityKey), strings.TrimSpace(real)
	if !turnStateVaultEnabled() || affinityKey == "" || real == "" || account == nil || account.ID() <= 0 {
		return ""
	}
	substitute := newCodexTurnStateSubstitute()
	if substitute == "" {
		return ""
	}
	turnStateVault.Store(affinityKey, &turnStateVaultEntry{real: real, substitute: substitute, accountID: account.ID(), expiresAt: time.Now().Add(turnStateVaultTTL)})
	turnStateVaultIssued.Add(1)
	if turnStateVaultWrites.Add(1)%turnStateVaultSweepEvery == 0 {
		now := time.Now()
		turnStateVault.Range(func(k, v any) bool {
			if entry, ok := v.(*turnStateVaultEntry); ok && now.After(entry.expiresAt) {
				turnStateVault.Delete(k)
			}
			return true
		})
	}
	return substitute
}

// resolveCodexTurnStateSubstitute 把回带的替身换回真实值：same 返回真实 token；
// cross（替身属于别的账号）与 unknown（不是本会话当前替身/已过期）返回空。
func resolveCodexTurnStateSubstitute(affinityKey string, account *auth.Account, inbound string) (string, turnStateEchoClass) {
	inbound = strings.TrimSpace(inbound)
	raw, ok := turnStateVault.Load(strings.TrimSpace(affinityKey))
	if !ok {
		return "", turnStateEchoUnknown
	}
	entry, ok := raw.(*turnStateVaultEntry)
	if !ok || time.Now().After(entry.expiresAt) || inbound == "" || inbound != entry.substitute {
		return "", turnStateEchoUnknown
	}
	if account == nil || account.ID() != entry.accountID {
		return "", turnStateEchoCross
	}
	return entry.real, turnStateEchoSame
}

// vaultCodexTurnStateEvent 改写 response.metadata / codex.response.metadata 事件里
// headers 对象的 x-codex-turn-state（官方客户端从这里读 token），其余事件原样返回。
func (h *Handler) vaultCodexTurnStateEvent(affinityKey string, account *auth.Account, eventType string, data []byte) []byte {
	if !turnStateVaultEnabled() {
		return data
	}
	switch strings.TrimSpace(eventType) {
	case "response.metadata", "codex.response.metadata":
	default:
		return data
	}
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	headers := gjson.GetBytes(data, "headers")
	if !headers.IsObject() {
		return data
	}
	name, token := "", ""
	headers.ForEach(func(k, v gjson.Result) bool {
		if strings.EqualFold(k.String(), "x-codex-turn-state") && v.Type == gjson.String && strings.TrimSpace(v.String()) != "" {
			name, token = k.String(), strings.TrimSpace(v.String())
			return false
		}
		return true
	})
	if token == "" {
		return data
	}
	substitute := issueCodexTurnStateSubstitute(affinityKey, account, token)
	if substitute == "" {
		return data
	}
	noteCodexTurnStateProvenance(affinityKey, account)
	updated, err := sjson.SetBytes(data, "headers."+name, substitute)
	if err != nil {
		return data
	}
	return updated
}
```

`proxy/codex_turn_state.go` — in `relayCodexTurnStateResponseHeader`, replace `c.Header(codexTurnStateHeader, token)` with:

```go
	if substitute := issueCodexTurnStateSubstitute(affinityKey, account, token); substitute != "" {
		token = substitute
	}
	c.Header(codexTurnStateHeader, token)
```

In `commitResponsesStreamAttempt`, right after `token = strings.TrimSpace(headers.Get(codexTurnStateHeader))` (inside `if headers != nil {`) add:

```go
		if substitute := issueCodexTurnStateSubstitute(affinityKey, account, token); substitute != "" {
			token = substitute
		}
```

`proxy/session_guards.go` — in `applyCodexTurnStateEchoPolicy`, replace the block from `class := h.classifyCodexTurnStateEcho(affinityKey, account)` through the `strip := ...` line with:

```go
	class := turnStateEchoUnknown
	restored := ""
	if turnStateVaultEnabled() {
		// 托管开启：只有本会话当前替身能换回真实值；其余（外来真实 token、旧替身）一律剥离。
		restored, class = resolveCodexTurnStateSubstitute(affinityKey, account, token)
		if class == turnStateEchoUnknown {
			turnStateVaultForeign.Add(1)
		}
	} else {
		class = h.classifyCodexTurnStateEcho(affinityKey, account)
	}
	strip := class == turnStateEchoCross || (class == turnStateEchoUnknown && (turnStateVaultEnabled() || CurrentRuntimeSettings().CodexTurnStateStrict))
	if restored != "" && class == turnStateEchoSame {
		if headers != nil && headers.Get(codexTurnStateHeader) != "" {
			headers.Set(codexTurnStateHeader, restored)
		}
		if bodyToken != "" {
			if updated, err := sjson.SetBytes(body, codexTurnStateBodyPath, restored); err == nil {
				body = updated
			}
		}
		turnStateVaultRestore.Add(1)
	}
```

(keep the existing strip/record/log code after it unchanged.) Add `Vault SessionGuardVaultCounters \`json:"vault"\`` to `SessionGuardTurnStateStatus` in `proxy/session_guard_status.go` and set it from `turnStateVaultCountersSnapshot()` in the snapshot.

`proxy/handler.go`: change `writeDeferredSSEData(streamWriter, &pendingFirstTokenEvents, sanitizeCapacityShedEventForClient(eventType, data), shouldDefer)` to

```go
					wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenEvents, h.vaultCodexTurnStateEvent(affinityKey, account, eventType, sanitizeCapacityShedEventForClient(eventType, data)), shouldDefer)
```

`proxy/responses_ws.go`: directly after `clientData = sanitizeCapacityShedEventForClient(eventType, clientData)` add `clientData = h.vaultCodexTurnStateEvent(affinityKey, account, eventType, clientData)`.

Wiring test: add assertions that `h\.vaultCodexTurnStateEvent\(affinityKey, account, eventType, ` appears exactly once in each of handler.go and responses_ws.go.

- [ ] **Step 4: Verify and commit**

Run: `gofmt -w proxy/turn_state_vault.go proxy/codex_turn_state.go proxy/session_guards.go proxy/handler.go proxy/responses_ws.go proxy/session_guard_status.go && go build ./... && go test ./proxy/ -count=1`. `gitnexus_impact` for `applyCodexTurnStateEchoPolicy`, `relayCodexTurnStateResponseHeader`; `gitnexus_detect_changes()`. Commit:

```bash
git add proxy/turn_state_vault.go proxy/turn_state_vault_test.go proxy/codex_turn_state.go proxy/session_guards.go proxy/handler.go proxy/responses_ws.go proxy/session_guard_status.go proxy/session_guards_wiring_test.go
git commit -m "feat(session-guards): keep real turn-state tokens in a gateway vault and hand clients substitutes"
```

---

### Task 7: Frontend (settings, runtime panel row, locked-sessions card, i18n)

**Files:**
- Modify: `frontend/src/types.ts` (SystemSettings: three optional fields after `codex_initial_session_max_age_seconds?: number`; `RuntimeStatusResponse.session_guards`: add `auto_lock` and `turn_state.vault`; new `SessionLockItem` interface), `frontend/src/api.ts` (`getSessionLocks`, `deleteSessionLock` next to `getRuntimeStatus`), `frontend/src/pages/Settings.tsx` (normalized defaults + form defaults + three controls after the max-age field), `frontend/src/pages/RuntimeStatus.tsx` (guards panel: two new rows; new card below it), locales ×3
- Test: extend `frontend/src/lib/sessionGuards.test.mjs`

- [ ] **Step 1: Failing guard test** — append to `frontend/src/lib/sessionGuards.test.mjs`:

```js
test('auto-lock and vault settings, locked-sessions card and copy exist', () => {
  for (const key of ['codex_session_auto_lock_enabled', 'codex_session_auto_lock_threshold', 'codex_turn_state_vault_enabled']) {
    assert.ok(typesSource.includes(`${key}?:`), `types.ts lacks ${key}`)
    assert.ok(settingsSource.includes(`${key}:`), `Settings.tsx defaults lack ${key}`)
  }
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_session_auto_lock_enabled'"))
  assert.ok(settingsSource.includes('autoSaveSettingsPatch({ codex_session_auto_lock_threshold: value })'))
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_turn_state_vault_enabled'"))
  assert.ok(runtimeSource.includes('api.getSessionLocks'), 'locked sessions must be loaded')
  assert.ok(runtimeSource.includes('api.deleteSessionLock('), 'unlock action missing')
  assert.ok(runtimeSource.includes('status.session_guards.auto_lock'), 'auto-lock row missing')
  assert.ok(typesSource.includes('export interface SessionLockItem'))
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of ['codexSessionAutoLock', 'codexSessionAutoLockDesc', 'codexSessionAutoLockThreshold', 'codexSessionAutoLockThresholdDesc', 'codexTurnStateVault', 'codexTurnStateVaultDesc']) {
      assert.equal(typeof locale.settings?.[key], 'string', `${name}.json settings.${key} missing`)
    }
    for (const key of ['autoLock', 'lockedSessions', 'lockedSessionsEmpty', 'unlock', 'unlocked', 'sessionPrefix', 'lockedAt', 'vault']) {
      assert.equal(typeof locale.runtime?.[key], 'string', `${name}.json runtime.${key} missing`)
    }
  }
})
```

- [ ] **Step 2: Run** `cd frontend && node --experimental-strip-types --test src/lib/sessionGuards.test.mjs` → FAIL.

- [ ] **Step 3: types.ts**

After `codex_initial_session_max_age_seconds?: number` add:

```ts
  codex_session_auto_lock_enabled?: boolean
  codex_session_auto_lock_threshold?: number
  codex_turn_state_vault_enabled?: boolean
```

In `session_guards`: inside `turn_state` add `vault: { issued: number; restored: number; foreign_stripped: number }`; after `borrow: {...}` add `auto_lock: { enabled: boolean; threshold: number; active_locks: number; locked_total: number; unlocked_total: number; streak_entries: number }`. Above `RuntimeStatusResponse` add:

```ts
export interface SessionLockItem {
  id: number
  session_id_prefix: string
  api_key_id: number
  account_id: number
  account_name: string
  error_message: string
  threshold: number
  source: string
  locked_at: ISODateString
}
```

- [ ] **Step 4: api.ts** — next to `getRuntimeStatus` add:

```ts
  getSessionLocks: (limit = 200) => request<{ locks: SessionLockItem[]; total: number }>(`/session-locks?limit=${limit}`),
  deleteSessionLock: (id: number) => request<MessageResponse>(`/session-locks/${id}`, { method: 'DELETE' }),
```

(import `SessionLockItem` with the other type imports.)

- [ ] **Step 5: Settings.tsx** — normalized defaults after the `codex_initial_session_max_age_seconds` line: `codex_session_auto_lock_enabled: cacheNormalized.codex_session_auto_lock_enabled ?? false,`, `codex_session_auto_lock_threshold: cacheNormalized.codex_session_auto_lock_threshold ?? 3,`, `codex_turn_state_vault_enabled: cacheNormalized.codex_turn_state_vault_enabled ?? true,`; form defaults likewise (`false`, `3`, `true`). Controls directly after the max-age `</SettingField>`:

```tsx
                    <SettingField label={t('settings.codexSessionAutoLock')} description={t('settings.codexSessionAutoLockDesc')} layout="switch" channels={CHANNELS_CODEX_ONLY}>
                      <Switch
                        checked={settingsForm.codex_session_auto_lock_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_session_auto_lock_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexSessionAutoLockThreshold')}
                      description={t('settings.codexSessionAutoLockThresholdDesc')}
                      className={cn(!settingsForm.codex_session_auto_lock_enabled && 'opacity-60')}
                      channels={CHANNELS_CODEX_ONLY}
                    >
                      <DraftNumberInput
                        min={1}
                        max={10000}
                        disabled={!settingsForm.codex_session_auto_lock_enabled}
                        value={settingsForm.codex_session_auto_lock_threshold ?? 3}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_session_auto_lock_threshold: value }))}
                        onValueCommit={(value) => void autoSaveSettingsPatch({ codex_session_auto_lock_threshold: value })}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexTurnStateVault')} description={t('settings.codexTurnStateVaultDesc')} layout="switch" channels={CHANNELS_CODEX_ONLY}>
                      <Switch
                        checked={settingsForm.codex_turn_state_vault_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_turn_state_vault_enabled', checked)}
                      />
                    </SettingField>
```

- [ ] **Step 6: RuntimeStatus.tsx**

Add two rows to the guards `StatusPanel` (after the borrow row):

```tsx
                    [t('runtime.vault'), `${t('runtime.vaultIssued')} ${formatNumber(status.session_guards.turn_state.vault.issued)} · ${t('runtime.vaultRestored')} ${formatNumber(status.session_guards.turn_state.vault.restored)} · ${t('runtime.vaultForeign')} ${formatNumber(status.session_guards.turn_state.vault.foreign_stripped)}`],
                    [t('runtime.autoLock'), `${status.session_guards.auto_lock.enabled ? t('common.enabled') : t('common.disabled')} (${status.session_guards.auto_lock.threshold}) · ${t('runtime.activeLocks')} ${formatNumber(status.session_guards.auto_lock.active_locks)} · ${t('runtime.lockedTotal')} ${formatNumber(status.session_guards.auto_lock.locked_total)} · ${t('runtime.unlockedTotal')} ${formatNumber(status.session_guards.auto_lock.unlocked_total)}`],
```

Below the guards panel (still inside the same container as the other panels), add the locked-sessions card. Load locks with a second `useDataLoader` (mirror the existing `loadRuntimeStatus` usage exactly) and render:

```tsx
              {status.session_guards && (
                <Card>
                  <CardContent className="space-y-3 p-4 sm:p-6">
                    <div className="flex items-center justify-between">
                      <h2 className="font-semibold">{t('runtime.lockedSessions')}</h2>
                      <Button variant="outline" size="sm" onClick={() => void reloadLocks()}>{t('common.refresh')}</Button>
                    </div>
                    {locks.length === 0 ? (
                      <p className="text-sm text-muted-foreground">{t('runtime.lockedSessionsEmpty')}</p>
                    ) : (
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>{t('runtime.sessionPrefix')}</TableHead>
                            <TableHead>API Key</TableHead>
                            <TableHead>{t('runtime.account')}</TableHead>
                            <TableHead>{t('runtime.error')}</TableHead>
                            <TableHead>{t('runtime.lockedAt')}</TableHead>
                            <TableHead />
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {locks.map((lock) => (
                            <TableRow key={lock.id}>
                              <TableCell className="font-mono text-xs">{lock.session_prefix_display ?? lock.session_id_prefix}</TableCell>
                              <TableCell>#{lock.api_key_id}</TableCell>
                              <TableCell>{lock.account_name || `#${lock.account_id}`}</TableCell>
                              <TableCell className="max-w-[320px] truncate text-xs" title={lock.error_message}>{lock.error_message || '-'}</TableCell>
                              <TableCell className="text-xs">{new Date(lock.locked_at).toLocaleString()}</TableCell>
                              <TableCell className="text-right">
                                <Button variant="outline" size="sm" onClick={() => void unlock(lock.id)}>{t('runtime.unlock')}</Button>
                              </TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                    )}
                  </CardContent>
                </Card>
              )}
```

(Drop the `session_prefix_display` fallback — use `lock.session_id_prefix` only.) `unlock` calls `api.deleteSessionLock(id)`, shows a toast/`t('runtime.unlocked')` the way the page reports other actions (if the page has no toast helper, `reloadLocks()` after success is enough), then reloads locks and the status. If `common.refresh` does not exist in the locales, use `runtime.refresh` if present or add `runtime.refreshLocks` to all three locales. Import `Table, TableBody, TableCell, TableHead, TableHeader, TableRow` from `@/components/ui/table`. If `Button` has no `size="sm"` variant in this codebase, omit `size`.

- [ ] **Step 7: i18n** — `settings` block (all three locales, after `codexInitialSessionMaxAgeDesc`):

zh:
```json
    "codexSessionAutoLock": "连续 500 自动锁定会话",
    "codexSessionAutoLockDesc": "默认关闭。同一会话在官方 Codex 账号上连续出现最终 500 达到次数后自动锁定，后续请求返回 400 session_blacklisted（retry: stop）。API 中转账号的请求不参与计数，内部重试不重复计，非 500 结果清零；保存本组设置会重置未触发的计数；关闭开关不解除已有锁定，解锁在「运行状态 → 已锁定会话」。",
    "codexSessionAutoLockThreshold": "连续错误次数",
    "codexSessionAutoLockThresholdDesc": "范围 1–10000，默认 3。",
    "codexTurnStateVault": "turn-state 托管（真实 token 不下发）",
    "codexTurnStateVaultDesc": "默认开启。上游铸造的 X-Codex-Turn-State 只留在网关，客户端收到随机替身（响应头与 response.metadata 事件均替换），回带时换回真实值；不是本会话当前替身的 token 一律剥离。防止用户把真实 token 带去别处再带回来污染原账号。多实例部署没有共享托管：另一实例无法还原替身，仅损失该轮粘性路由。",
```

en:
```json
    "codexSessionAutoLock": "Auto-lock sessions after consecutive 500s",
    "codexSessionAutoLockDesc": "Off by default. When a session hits the configured number of consecutive final HTTP 500s on official Codex accounts it is locked and later requests get 400 session_blacklisted (retry: stop). Requests served by API relay accounts never count, internal retries count once, any non-500 result resets the streak; saving these settings resets pending streaks; turning the switch off does not unlock existing sessions — unlock under Runtime status → Locked sessions.",
    "codexSessionAutoLockThreshold": "Consecutive errors",
    "codexSessionAutoLockThresholdDesc": "1–10000, default 3.",
    "codexTurnStateVault": "Turn-state vault (never expose the real token)",
    "codexTurnStateVaultDesc": "On by default. The upstream X-Codex-Turn-State stays inside the gateway; clients receive a random substitute (response header and response.metadata event) that is swapped back on the way out. Any token that is not this session's current substitute is stripped. Prevents a user from carrying the real token elsewhere and bringing it back to poison the minting account. Multi-instance deployments have no shared vault: another instance cannot restore a substitute and only loses that turn's sticky routing.",
```

zh-TW:
```json
    "codexSessionAutoLock": "連續 500 自動鎖定工作階段",
    "codexSessionAutoLockDesc": "預設關閉。同一工作階段在官方 Codex 帳號上連續出現最終 500 達到次數後自動鎖定，後續請求回傳 400 session_blacklisted（retry: stop）。API 中轉帳號的請求不參與計數，內部重試不重複計，非 500 結果清零；儲存本組設定會重設未觸發的計數；關閉開關不解除既有鎖定，解鎖在「執行狀態 → 已鎖定工作階段」。",
    "codexSessionAutoLockThreshold": "連續錯誤次數",
    "codexSessionAutoLockThresholdDesc": "範圍 1–10000，預設 3。",
    "codexTurnStateVault": "turn-state 託管（真實 token 不下發）",
    "codexTurnStateVaultDesc": "預設開啟。上游鑄造的 X-Codex-Turn-State 只留在閘道，用戶端收到隨機替身（回應標頭與 response.metadata 事件均替換），回帶時換回真實值；不是本工作階段目前替身的 token 一律剝離。防止使用者把真實 token 帶去別處再帶回來污染原帳號。多實例部署沒有共享託管：另一實例無法還原替身，僅損失該輪黏性路由。",
```

`runtime` block (all three locales) add:

zh: `"vault": "turn-state 托管", "vaultIssued": "已下发替身", "vaultRestored": "已还原", "vaultForeign": "外来剥离", "autoLock": "自动锁定", "activeLocks": "当前锁定", "lockedTotal": "累计锁定", "unlockedTotal": "累计解锁", "lockedSessions": "已锁定会话", "lockedSessionsEmpty": "暂无被自动锁定的会话", "unlock": "解锁", "unlocked": "已解锁", "sessionPrefix": "会话前缀", "account": "账号", "error": "错误", "lockedAt": "锁定时间", "refreshLocks": "刷新"`

en: `"vault": "Turn-state vault", "vaultIssued": "issued", "vaultRestored": "restored", "vaultForeign": "foreign stripped", "autoLock": "Auto-lock", "activeLocks": "active", "lockedTotal": "locked total", "unlockedTotal": "unlocked total", "lockedSessions": "Locked sessions", "lockedSessionsEmpty": "No auto-locked sessions", "unlock": "Unlock", "unlocked": "Unlocked", "sessionPrefix": "Session prefix", "account": "Account", "error": "Error", "lockedAt": "Locked at", "refreshLocks": "Refresh"`

zh-TW: `"vault": "turn-state 託管", "vaultIssued": "已下發替身", "vaultRestored": "已還原", "vaultForeign": "外來剝離", "autoLock": "自動鎖定", "activeLocks": "目前鎖定", "lockedTotal": "累計鎖定", "unlockedTotal": "累計解鎖", "lockedSessions": "已鎖定工作階段", "lockedSessionsEmpty": "暫無被自動鎖定的工作階段", "unlock": "解鎖", "unlocked": "已解鎖", "sessionPrefix": "工作階段前綴", "account": "帳號", "error": "錯誤", "lockedAt": "鎖定時間", "refreshLocks": "重新整理"`

(Use `t('runtime.refreshLocks')` for the card's refresh button.) If any of these `runtime.*` keys already exist with a different meaning in zh/en, keep the existing one and reuse it.

- [ ] **Step 8: Verify and commit**

Run: `cd frontend && npm test && npm run typecheck`. `gitnexus_detect_changes()`. Commit:

```bash
git add frontend/src/types.ts frontend/src/api.ts frontend/src/pages/Settings.tsx frontend/src/pages/RuntimeStatus.tsx frontend/src/locales/zh.json frontend/src/locales/en.json frontend/src/locales/zh-TW.json frontend/src/lib/sessionGuards.test.mjs
git commit -m "feat(session-guards): settings and runtime UI for auto-lock and turn-state vault"
```

---

### Task 8: Docs, full verification, commit

**Files:**
- Modify: `docs/session-guards.md` (new sections), `docs/CONFIGURATION.md` (pointer sentence already exists — no change unless the list of guards is enumerated there)

- [ ] **Step 1: Docs** — in `docs/session-guards.md`, add two rows to the table:

```markdown
| 连续 500 自动锁定会话 + 连续错误次数 | 关 / 3 | 同一会话（会话 ID + API Key）在官方 Codex 账号上连续最终 500 达阈值即锁定：400 `session_blacklisted`（`retry: stop`）。中转账号请求不计、内部重试不重复计、非 500 清零；锁落库、重启保留；解锁在「运行状态 → 已锁定会话」，接口 `GET/DELETE /api/admin/session-locks` |
| turn-state 托管 | 开 | 真实 `X-Codex-Turn-State` 只留在网关（1 小时），客户端拿随机替身（`c2a-ts-v1.…`，响应头与 `response.metadata` 事件都替换），回带时换回真实值；不是本会话当前替身的 token 一律剥离 |
```

and under 已知代价 add:

```markdown
- 托管是进程内存：多实例部署下另一实例无法还原替身，只损失该轮粘性路由；重启后客户端下一轮拿到新替身。
- 自动锁定的连击只在内存，重启或保存设置清零；锁表跨实例只在对方重启后可见。
- 首次会话准入现在在选号之后判定，落到中转账号的会话不再受影响。
```

- [ ] **Step 2: Full verification** (foreground):

```bash
gofmt -l database auth proxy admin | grep -v -E '^(auth/expiry_urgency_test.go|auth/proxy_pool.go|auth/proxy_pool_integration.go|auth/refresh_scheduler.go|proxy/continuity_test.go|proxy/payload_rules.go|proxy/resin.go|proxy/wsrelay/message.go|proxy/wsrelay/message_test.go|admin/codex_fingerprint_mode_test.go)$'
go vet ./proxy/ ./auth/ ./database/ ./admin/
go test ./database/ ./auth/ ./admin/ ./proxy/ -count=1
cd frontend && npm test && npm run typecheck && cd ..
```

Expected: gofmt prints nothing; vet clean; all PASS; frontend PASS + tsc clean.

- [ ] **Step 3: Commit**

`gitnexus_detect_changes()`, then:

```bash
git add docs/session-guards.md
git commit -m "docs(session-guards): auto-lock, admission scope and turn-state vault"
```

---

## Self-Review

- Spec §1 (admission relay exemption) → Task 5. §2 auto-lock: settings → Task 1; table → Task 2; streaks/lock/entry/counters/reset → Task 3; admin API → Task 4; UI → Task 7; docs → Task 8. §3 vault: setting → Task 1; vault module, header/event substitution, restoration, counters → Task 6; UI switch/counters → Task 7; docs → Task 8. ✔
- Placeholder scan: all steps carry code; the only "mirror X" instructions point at named existing code (`scanPromptConversationLock`, the `bindContinuousRetrySessionAffinityWithGuard` early return, `ListAccountGroups` helpers).
- Type consistency: `SessionAutoLockInput`/`SessionAutoLock` fields match between Tasks 2/3/4; `resolveCodexTurnStateSubstitute` returns `(string, turnStateEchoClass)` in Task 6 code and tests; `SessionGuardAutoLockStatus`/`SessionGuardVaultCounters` JSON tags match the TS types in Task 7; settings JSON keys match across Tasks 1/7.
