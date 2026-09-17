# Codex Session Guards Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add four default-off, independently testable guards against cross-account Codex session state (turn-state observability, strict turn-state stripping, no-borrow session affinity, initial-session UUIDv7 age admission) plus a runtime-status panel for A/B testing.

**Architecture:** All logic stays on the current upstream APIs. Turn-state classification/policy lives in a new `proxy/session_guards.go` and is wired into the two existing attempt loops (`proxy/handler.go` HTTP, `proxy/responses_ws.go` WS) at the point where `guardCodexTurnStateEcho` runs today. No-borrow is a store-level `allowBorrow` flag threaded through `nextForSessionWithFilter` and the wait loop. Initial-session admission is a pure function over `requestSessionIdentity` + binding lookup, called right after `affinityKey` is computed in both entry points. Settings follow the existing two patterns exactly: `CodexTelemetryTimingDebug` (RuntimeSettings) and `SessionSlotBuffer*` (store atomics). Counters are in-process and surfaced through `/api/admin/runtime`.

**Tech Stack:** Go 1.26 (stdlib `testing`, gin, gjson/sjson, google/uuid), React + TypeScript frontend (`node --test` `.mjs` source-guard tests, i18next), PostgreSQL + SQLite migrations.

**Spec:** `docs/superpowers/specs/2026-09-17-codex-session-guards-design.md`

## Global Constraints

- Project CLAUDE.md: run `gitnexus_impact({target, direction:"upstream"})` before editing any existing function; run `gitnexus_detect_changes()` before every commit; warn on HIGH/CRITICAL.
- Frontend: read `DESIGN.md` before touching any `.tsx`; use `components/ui/` controls only (`Switch`, `DraftNumberInput`, `SettingField` as already used in `Settings.tsx`); every new string goes to `zh.json`, `en.json`, `zh-TW.json`; new settings blocks need an assertion in a source-guard test; run `cd frontend && npm test && npm run typecheck`.
- All new behavior is **default off** except passive counters. Defaults verbatim: `codex_turn_state_strict=false`, `codex_session_no_borrow_enabled=false`, `codex_session_no_borrow_hold_seconds=20` (1–30), `codex_initial_session_admission_enabled=false`, `codex_initial_session_max_age_seconds=180` (1–86400).
- Initial-session `future` tolerance is a constant 30 s; non-UUID / non-v7 IDs are counted as `invalid` but **never rejected**.
- Go tests use stdlib `testing` only (no testify). Test names follow the package style `TestXxx`.
- Commit messages: `feat(session-guards): ...` / `test(session-guards): ...`; one commit per task.

---

### Task 1: Settings persistence (DB columns, SystemSettings, RuntimeSettings, store atomics)

**Files:**
- Modify: `database/postgres.go` (migration block near line 1445; `SystemSettings` struct near line 2380; normalizer near line 2541; `GetSystemSettings` SELECT/Scan near lines 2678–2768; `UpdateSystemSettings` column list / VALUES / ON CONFLICT / args near lines 3012–3194)
- Modify: `database/sqlite.go:335` (CREATE TABLE column list) and `database/sqlite.go:645` (ensure-column list)
- Modify: `proxy/runtime_config.go` (`RuntimeSettings` struct near line 74; `DefaultRuntimeSettings` near line 181; `NormalizeRuntimeSettings` near line 267; `ApplyRuntimeSettingsFromSystem` near line 347)
- Modify: `auth/store.go` (struct field near line 3609; `NewStore` init near line 4141; new setters near line 7843)
- Test: `database/session_guard_settings_test.go`, `auth/session_no_borrow_settings_test.go`

**Interfaces:**
- Produces: `database.SystemSettings` fields `CodexTurnStateStrict bool`, `CodexSessionNoBorrowEnabled bool`, `CodexSessionNoBorrowHoldSeconds int`, `CodexInitialSessionAdmissionEnabled bool`, `CodexInitialSessionMaxAgeSeconds int`
- Produces: `database.NormalizeSessionNoBorrowHoldSeconds(int) int` (≤0 → 20, >30 → 30), `database.NormalizeCodexInitialSessionMaxAgeSeconds(int) int` (<1 or >86400 → 180)
- Produces: `proxy.RuntimeSettings` fields `CodexTurnStateStrict bool`, `CodexInitialSessionAdmissionEnabled bool`, `CodexInitialSessionMaxAgeSeconds int`
- Produces: `(*auth.Store).SetSessionNoBorrow(enabled bool, hold time.Duration)`, `(*auth.Store).SessionNoBorrowEnabled() bool`, `(*auth.Store).SessionNoBorrowHold() time.Duration`

- [ ] **Step 1: Write the failing SQLite roundtrip test**

Create `database/session_guard_settings_test.go`:

```go
package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteSessionGuardSettingsRoundtrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "guards.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		t.Fatalf("insert defaults: %v", err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		t.Fatalf("GetSystemSettings: %#v, err = %v", settings, err)
	}
	if settings.CodexTurnStateStrict || settings.CodexSessionNoBorrowEnabled || settings.CodexInitialSessionAdmissionEnabled {
		t.Fatalf("guard switches must default off: %#v", settings)
	}
	if settings.CodexSessionNoBorrowHoldSeconds != 20 || settings.CodexInitialSessionMaxAgeSeconds != 180 {
		t.Fatalf("guard defaults = hold %d, max age %d; want 20, 180", settings.CodexSessionNoBorrowHoldSeconds, settings.CodexInitialSessionMaxAgeSeconds)
	}

	settings.CodexTurnStateStrict = true
	settings.CodexSessionNoBorrowEnabled = true
	settings.CodexSessionNoBorrowHoldSeconds = 25
	settings.CodexInitialSessionAdmissionEnabled = true
	settings.CodexInitialSessionMaxAgeSeconds = 600
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		t.Fatalf("reload: %#v, err = %v", settings, err)
	}
	if !settings.CodexTurnStateStrict || !settings.CodexSessionNoBorrowEnabled || !settings.CodexInitialSessionAdmissionEnabled {
		t.Fatalf("guard switches did not persist: %#v", settings)
	}
	if settings.CodexSessionNoBorrowHoldSeconds != 25 || settings.CodexInitialSessionMaxAgeSeconds != 600 {
		t.Fatalf("guard numbers did not persist: hold %d, max age %d", settings.CodexSessionNoBorrowHoldSeconds, settings.CodexInitialSessionMaxAgeSeconds)
	}

	settings.CodexSessionNoBorrowHoldSeconds = 999
	settings.CodexInitialSessionMaxAgeSeconds = 0
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings(out of range): %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		t.Fatalf("reload: %#v, err = %v", settings, err)
	}
	if settings.CodexSessionNoBorrowHoldSeconds != 30 || settings.CodexInitialSessionMaxAgeSeconds != 180 {
		t.Fatalf("out-of-range values must normalize on write: hold %d, max age %d", settings.CodexSessionNoBorrowHoldSeconds, settings.CodexInitialSessionMaxAgeSeconds)
	}
}

func TestNormalizeSessionGuardNumbers(t *testing.T) {
	cases := []struct{ in, hold, age int }{
		{0, 20, 180}, {-5, 20, 180}, {1, 1, 1}, {30, 30, 30}, {31, 30, 31}, {86400, 30, 86400}, {86401, 30, 180},
	}
	for _, tc := range cases {
		if got := NormalizeSessionNoBorrowHoldSeconds(tc.in); got != tc.hold {
			t.Errorf("NormalizeSessionNoBorrowHoldSeconds(%d) = %d, want %d", tc.in, got, tc.hold)
		}
		if got := NormalizeCodexInitialSessionMaxAgeSeconds(tc.in); got != tc.age {
			t.Errorf("NormalizeCodexInitialSessionMaxAgeSeconds(%d) = %d, want %d", tc.in, got, tc.age)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./database/ -run 'TestSQLiteSessionGuardSettingsRoundtrip|TestNormalizeSessionGuardNumbers' -count=1`
Expected: compile error — `settings.CodexTurnStateStrict undefined`, `NormalizeSessionNoBorrowHoldSeconds undefined`.

- [ ] **Step 3: Add the normalizers and SystemSettings fields**

In `database/postgres.go`, directly after `NormalizeSessionSlotBufferSeconds` (line ~2549) add:

```go
// NormalizeSessionNoBorrowHoldSeconds bounds how long a bound session waits for
// its own account before the capacity spillover fallback is allowed again.
// The scheduler wait budget is 30s, so the hold is capped there.
func NormalizeSessionNoBorrowHoldSeconds(seconds int) int {
	if seconds <= 0 {
		return 20
	}
	if seconds > 30 {
		return 30
	}
	return seconds
}

// NormalizeCodexInitialSessionMaxAgeSeconds bounds the UUIDv7 age accepted for a
// Codex session that has no account binding yet (1s..24h, default 180s).
func NormalizeCodexInitialSessionMaxAgeSeconds(seconds int) int {
	if seconds < 1 || seconds > 86400 {
		return 180
	}
	return seconds
}
```

In the `SystemSettings` struct, directly after the line `CodexTelemetryTimingDebug          bool` (line ~2380) add:

```go
	CodexTurnStateStrict                bool // 来源未知的 X-Codex-Turn-State 回带也剥离，并按帧携带
	CodexSessionNoBorrowEnabled         bool // 绑定账号并发满时先等待而不是借用其他账号
	CodexSessionNoBorrowHoldSeconds     int  // 等待多久后才允许借用，1..30，默认 20
	CodexInitialSessionAdmissionEnabled bool // 无绑定的 Codex 会话按 UUIDv7 年龄准入
	CodexInitialSessionMaxAgeSeconds    int  // 首次会话 ID 最大年龄，1..86400，默认 180
```

(gofmt will realign the struct; run `gofmt -w database/postgres.go` after editing.)

- [ ] **Step 4: Add the migrations**

In `database/postgres.go` migration block, directly after the two `codex_telemetry_timing_debug` ALTER lines (line ~1446) add:

```sql
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_turn_state_strict BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_session_no_borrow_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_session_no_borrow_hold_seconds INT DEFAULT 20;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_initial_session_admission_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_initial_session_max_age_seconds INT DEFAULT 180;
```

In `database/sqlite.go` CREATE TABLE list, directly after `codex_telemetry_timing_debug INTEGER DEFAULT 0,` (line 335) add:

```sql
					codex_turn_state_strict INTEGER DEFAULT 0,
					codex_session_no_borrow_enabled INTEGER DEFAULT 0,
					codex_session_no_borrow_hold_seconds INTEGER DEFAULT 20,
					codex_initial_session_admission_enabled INTEGER DEFAULT 0,
					codex_initial_session_max_age_seconds INTEGER DEFAULT 180,
```

In the ensure-column slice, directly after `{"system_settings", "codex_telemetry_timing_debug", "INTEGER DEFAULT 0"},` (line 645) add:

```go
		{"system_settings", "codex_turn_state_strict", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_session_no_borrow_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_session_no_borrow_hold_seconds", "INTEGER DEFAULT 20"},
		{"system_settings", "codex_initial_session_admission_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_initial_session_max_age_seconds", "INTEGER DEFAULT 180"},
```

- [ ] **Step 5: Wire GetSystemSettings**

In the SELECT (line ~2682) change the last line from

```sql
		       COALESCE(codex_telemetry_timing_debug, false)
```
to
```sql
		       COALESCE(codex_telemetry_timing_debug, false),
		       COALESCE(codex_turn_state_strict, false),
		       COALESCE(codex_session_no_borrow_enabled, false),
		       COALESCE(codex_session_no_borrow_hold_seconds, 20),
		       COALESCE(codex_initial_session_admission_enabled, false),
		       COALESCE(codex_initial_session_max_age_seconds, 180)
```

In the `.Scan(` list (line ~2766) change `&s.CodexTelemetryTimingDebug,` to

```go
		&s.CodexTelemetryTimingDebug,
		&s.CodexTurnStateStrict,
		&s.CodexSessionNoBorrowEnabled,
		&s.CodexSessionNoBorrowHoldSeconds,
		&s.CodexInitialSessionAdmissionEnabled,
		&s.CodexInitialSessionMaxAgeSeconds,
```

Directly after the existing post-scan normalization `s.SessionSlotBufferSeconds = NormalizeSessionSlotBufferSeconds(s.SessionSlotBufferSeconds)` (line ~2802) add:

```go
	s.CodexSessionNoBorrowHoldSeconds = NormalizeSessionNoBorrowHoldSeconds(s.CodexSessionNoBorrowHoldSeconds)
	s.CodexInitialSessionMaxAgeSeconds = NormalizeCodexInitialSessionMaxAgeSeconds(s.CodexInitialSessionMaxAgeSeconds)
```

- [ ] **Step 6: Wire UpdateSystemSettings (positional parameters shift by 5)**

In the INSERT column list (line ~3016) change

```sql
					codex_telemetry_timing_debug
					)
```
to
```sql
					codex_telemetry_timing_debug,
					codex_turn_state_strict,
					codex_session_no_borrow_enabled,
					codex_session_no_borrow_hold_seconds,
					codex_initial_session_admission_enabled,
					codex_initial_session_max_age_seconds
					)
```

In the `VALUES (...)` line append `, $124, $125, $126, $127, $128` after `$123` (before the closing `)`).

In the ON CONFLICT block change the two CASE WHEN placeholders: `CASE WHEN $124 THEN system_settings.prompt_filter_custom_patterns` → `CASE WHEN $129 THEN ...` (line 3058) and `CASE WHEN $125 THEN system_settings.prompt_filter_review_api_key` → `CASE WHEN $130 THEN ...` (line 3061). Then change the last SET line (line ~3139)

```sql
					codex_telemetry_timing_debug = EXCLUDED.codex_telemetry_timing_debug
```
to
```sql
					codex_telemetry_timing_debug = EXCLUDED.codex_telemetry_timing_debug,
					codex_turn_state_strict = EXCLUDED.codex_turn_state_strict,
					codex_session_no_borrow_enabled = EXCLUDED.codex_session_no_borrow_enabled,
					codex_session_no_borrow_hold_seconds = EXCLUDED.codex_session_no_borrow_hold_seconds,
					codex_initial_session_admission_enabled = EXCLUDED.codex_initial_session_admission_enabled,
					codex_initial_session_max_age_seconds = EXCLUDED.codex_initial_session_max_age_seconds
```

In the args list (line ~3192) change

```go
		s.CodexTelemetryTimingDebug,
		s.PreservePromptFilterCustomPatterns,
		s.PreservePromptFilterReviewAPIKey)
```
to
```go
		s.CodexTelemetryTimingDebug,
		s.CodexTurnStateStrict,
		s.CodexSessionNoBorrowEnabled,
		NormalizeSessionNoBorrowHoldSeconds(s.CodexSessionNoBorrowHoldSeconds),
		s.CodexInitialSessionAdmissionEnabled,
		NormalizeCodexInitialSessionMaxAgeSeconds(s.CodexInitialSessionMaxAgeSeconds),
		s.PreservePromptFilterCustomPatterns,
		s.PreservePromptFilterReviewAPIKey)
```

Verify with `grep -c '\$1[0-9][0-9]' database/postgres.go` that no other `$124`/`$125` reference remains in this statement: `sed -n '3000,3200p' database/postgres.go | grep -n 'CASE WHEN'` must show only `$129` and `$130`.

- [ ] **Step 7: Run the database tests**

Run: `go test ./database/ -run 'TestSQLiteSessionGuardSettingsRoundtrip|TestNormalizeSessionGuardNumbers|TestSQLiteCodexTelemetrySettingRoundtrip' -count=1`
Expected: PASS (the telemetry test guards that the positional shift did not break neighbours).

- [ ] **Step 8: Write the failing store settings test**

Create `auth/session_no_borrow_settings_test.go`:

```go
package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestSessionNoBorrowSettingsDefaultOffAndHotUpdate(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	if store.SessionNoBorrowEnabled() {
		t.Fatal("no-borrow must default off")
	}
	if got := store.SessionNoBorrowHold(); got != 20*time.Second {
		t.Fatalf("default hold = %s, want 20s", got)
	}
	store.SetSessionNoBorrow(true, 5*time.Second)
	if !store.SessionNoBorrowEnabled() || store.SessionNoBorrowHold() != 5*time.Second {
		t.Fatalf("hot update lost: enabled=%v hold=%s", store.SessionNoBorrowEnabled(), store.SessionNoBorrowHold())
	}
	store.SetSessionNoBorrow(true, 0)
	if got := store.SessionNoBorrowHold(); got != 20*time.Second {
		t.Fatalf("zero hold must normalize to default: %s", got)
	}
	store.SetSessionNoBorrow(true, 2*time.Minute)
	if got := store.SessionNoBorrowHold(); got != 30*time.Second {
		t.Fatalf("hold must cap at 30s: %s", got)
	}
}

func TestNewStoreLoadsSessionNoBorrowFromSystemSettings(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, CodexSessionNoBorrowEnabled: true, CodexSessionNoBorrowHoldSeconds: 12})
	if !store.SessionNoBorrowEnabled() || store.SessionNoBorrowHold() != 12*time.Second {
		t.Fatalf("NewStore did not load no-borrow settings: enabled=%v hold=%s", store.SessionNoBorrowEnabled(), store.SessionNoBorrowHold())
	}
}
```

- [ ] **Step 9: Run it to verify it fails**

Run: `go test ./auth/ -run 'TestSessionNoBorrow|TestNewStoreLoadsSessionNoBorrow' -count=1`
Expected: compile error — `store.SessionNoBorrowEnabled undefined`.

- [ ] **Step 10: Add store atomics, setters, and NewStore init**

Run `gitnexus_impact({target: "NewStore", direction: "upstream"})` and report the blast radius before editing.

In `auth/store.go` `Store` struct, directly after `sessionSlotBufferEnabled      atomic.Bool` (line ~3609) add:

```go
	sessionNoBorrowEnabled        atomic.Bool
	sessionNoBorrowHoldNS         atomic.Int64
```

In `NewStore`, directly after the line `s.SetSessionSlotBuffer(time.Duration(database.NormalizeSessionSlotBufferSeconds(settings.SessionSlotBufferSeconds)) * time.Second)` (line ~4142) add:

```go
	s.SetSessionNoBorrow(settings.CodexSessionNoBorrowEnabled, time.Duration(settings.CodexSessionNoBorrowHoldSeconds)*time.Second)
```

Directly after `func (s *Store) SessionSlotBufferEnabled() bool { ... }` (line ~7843) add:

```go
// SetSessionNoBorrow hot-updates the no-borrow policy: while enabled, a bound
// session whose account is at concurrency waits up to hold before the capacity
// spillover fallback is allowed. hold is normalized to 1..30s (default 20s).
func (s *Store) SetSessionNoBorrow(enabled bool, hold time.Duration) {
	if s == nil {
		return
	}
	seconds := database.NormalizeSessionNoBorrowHoldSeconds(int(hold / time.Second))
	s.sessionNoBorrowEnabled.Store(enabled)
	s.sessionNoBorrowHoldNS.Store(int64(time.Duration(seconds) * time.Second))
}

func (s *Store) SessionNoBorrowEnabled() bool {
	if s == nil {
		return false
	}
	return s.sessionNoBorrowEnabled.Load()
}

func (s *Store) SessionNoBorrowHold() time.Duration {
	if s == nil {
		return 20 * time.Second
	}
	if ns := s.sessionNoBorrowHoldNS.Load(); ns > 0 {
		return time.Duration(ns)
	}
	return 20 * time.Second
}
```

- [ ] **Step 11: Add RuntimeSettings fields**

In `proxy/runtime_config.go` `RuntimeSettings` struct, directly after `CodexTelemetryTimingDebug bool` (line 74) add:

```go
	// CodexTurnStateStrict 来源未知的 X-Codex-Turn-State 回带也剥离，并让上游 WS 按帧携带 token。
	CodexTurnStateStrict bool
	// CodexInitialSessionAdmissionEnabled 无绑定的 Codex 会话按 UUIDv7 年龄准入。
	CodexInitialSessionAdmissionEnabled bool
	// CodexInitialSessionMaxAgeSeconds 首次会话 ID 允许的最大年龄（秒），1..86400，默认 180。
	CodexInitialSessionMaxAgeSeconds int
```

In `DefaultRuntimeSettings()` directly after `CodexTelemetryTimingDebug:        false,` add:

```go
		CodexTurnStateStrict:                false,
		CodexInitialSessionAdmissionEnabled: false,
		CodexInitialSessionMaxAgeSeconds:    180,
```

In `NormalizeRuntimeSettings` (line ~267), as the first statement of the function body add:

```go
	settings.CodexInitialSessionMaxAgeSeconds = database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds)
```

In `ApplyRuntimeSettingsFromSystem` directly after `next.CodexTelemetryTimingDebug = settings.CodexTelemetryTimingDebug` add:

```go
		next.CodexTurnStateStrict = settings.CodexTurnStateStrict
		next.CodexInitialSessionAdmissionEnabled = settings.CodexInitialSessionAdmissionEnabled
		next.CodexInitialSessionMaxAgeSeconds = database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds)
```

- [ ] **Step 12: Run tests and build**

Run: `gofmt -l database auth proxy && go build ./... && go test ./database/ ./auth/ -run 'SessionGuard|SessionNoBorrow|NormalizeSessionGuard|CodexTelemetrySetting' -count=1`
Expected: gofmt prints nothing; build OK; PASS.

- [ ] **Step 13: Commit**

Run `gitnexus_detect_changes()` and confirm only the settings/persistence symbols are affected, then:

```bash
git add database/postgres.go database/sqlite.go database/session_guard_settings_test.go proxy/runtime_config.go auth/store.go auth/session_no_borrow_settings_test.go docs/superpowers
git commit -m "feat(session-guards): persist turn-state, no-borrow and initial-session settings"
```

---

### Task 2: Admin settings API

**Files:**
- Modify: `admin/handler.go` (`settingsResponse` struct near line 9223; `updateSettingsReq` near line 9392; `GetSettings` literal near line 10229; `UpdateSettings` existing-settings fallback near lines 10545–10580; runtime apply block near line 11300; `SystemSettings` persist literal near line 11721; store hot-apply near line 11798; second/third response literals near lines 12009 and 12073)
- Test: `admin/session_guard_settings_test.go`

**Interfaces:**
- Consumes: Task 1 fields/normalizers/setters.
- Produces: JSON keys `codex_turn_state_strict`, `codex_session_no_borrow_enabled`, `codex_session_no_borrow_hold_seconds`, `codex_initial_session_admission_enabled`, `codex_initial_session_max_age_seconds` on `GET/PUT /api/admin/settings`.

- [ ] **Step 1: Write the failing admin test**

Look at how `admin/handler_test.go` builds a settings handler with a SQLite DB (search `TestUpdateSettings` there and copy its helper for creating `db` and `h`; the helper name is whatever that file uses — reuse it verbatim). Then create `admin/session_guard_settings_test.go` with this body, adapting only the two helper calls:

```go
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestSessionGuardSettingsRoundtrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newSettingsTestHandler(t) // reuse the existing helper from handler_test.go
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })

	get := func() map[string]any {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
		h.GetSettings(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings = %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
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
	if initial["codex_turn_state_strict"] != false || initial["codex_session_no_borrow_enabled"] != false || initial["codex_initial_session_admission_enabled"] != false {
		t.Fatalf("switches must default off: %v", initial)
	}
	if initial["codex_session_no_borrow_hold_seconds"] != float64(20) || initial["codex_initial_session_max_age_seconds"] != float64(180) {
		t.Fatalf("numbers must default 20/180: %v", initial)
	}

	if code := put(`{"codex_turn_state_strict":true,"codex_session_no_borrow_enabled":true,"codex_session_no_borrow_hold_seconds":25,"codex_initial_session_admission_enabled":true,"codex_initial_session_max_age_seconds":600}`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	after := get()
	if after["codex_turn_state_strict"] != true || after["codex_session_no_borrow_enabled"] != true || after["codex_initial_session_admission_enabled"] != true {
		t.Fatalf("switches did not persist: %v", after)
	}
	if after["codex_session_no_borrow_hold_seconds"] != float64(25) || after["codex_initial_session_max_age_seconds"] != float64(600) {
		t.Fatalf("numbers did not persist: %v", after)
	}
	runtime := proxy.CurrentRuntimeSettings()
	if !runtime.CodexTurnStateStrict || !runtime.CodexInitialSessionAdmissionEnabled || runtime.CodexInitialSessionMaxAgeSeconds != 600 {
		t.Fatalf("runtime settings not hot-applied: %#v", runtime)
	}
	if !h.store.SessionNoBorrowEnabled() || h.store.SessionNoBorrowHold() != 25*time.Second {
		t.Fatalf("store no-borrow not hot-applied: %v %s", h.store.SessionNoBorrowEnabled(), h.store.SessionNoBorrowHold())
	}

	if code := put(`{"codex_session_no_borrow_hold_seconds":31}`); code != http.StatusOK {
		t.Fatalf("PUT hold 31 = %d", code)
	}
	if get()["codex_session_no_borrow_hold_seconds"] != float64(30) {
		t.Fatal("hold must clamp to 30")
	}
	if code := put(`{"codex_initial_session_max_age_seconds":0}`); code != http.StatusOK {
		t.Fatalf("PUT max age 0 = %d", code)
	}
	if get()["codex_initial_session_max_age_seconds"] != float64(180) {
		t.Fatal("max age 0 must normalize to 180")
	}
}
```

If `handler_test.go` has no reusable helper that returns a handler with `store` + SQLite `db` + `rateLimiter`, write `newSettingsTestHandler` in the new test file by copying the setup lines from the nearest existing `TestUpdateSettings*` test (it must set `h.db`, `h.store`, `h.rateLimiter`, `h.cacheCfgStore` exactly as that test does).

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./admin/ -run TestSessionGuardSettingsRoundtrip -count=1`
Expected: FAIL — `switches must default off` map lacks the keys (they decode as nil).

- [ ] **Step 3: Add the fields to both structs**

In `settingsResponse` (line ~9223), directly after `CodexTelemetryTimingDebug          bool                             \`json:"codex_telemetry_timing_debug"\`` add:

```go
	CodexTurnStateStrict                bool `json:"codex_turn_state_strict"`
	CodexSessionNoBorrowEnabled         bool `json:"codex_session_no_borrow_enabled"`
	CodexSessionNoBorrowHoldSeconds     int  `json:"codex_session_no_borrow_hold_seconds"`
	CodexInitialSessionAdmissionEnabled bool `json:"codex_initial_session_admission_enabled"`
	CodexInitialSessionMaxAgeSeconds    int  `json:"codex_initial_session_max_age_seconds"`
```

In `updateSettingsReq` (line ~9392), directly after `CodexTelemetryTimingDebug           *bool                            \`json:"codex_telemetry_timing_debug"\`` add:

```go
	CodexTurnStateStrict                *bool `json:"codex_turn_state_strict"`
	CodexSessionNoBorrowEnabled         *bool `json:"codex_session_no_borrow_enabled"`
	CodexSessionNoBorrowHoldSeconds     *int  `json:"codex_session_no_borrow_hold_seconds"`
	CodexInitialSessionAdmissionEnabled *bool `json:"codex_initial_session_admission_enabled"`
	CodexInitialSessionMaxAgeSeconds    *int  `json:"codex_initial_session_max_age_seconds"`
```

- [ ] **Step 4: Fill the three response literals**

At each of the three places that contain `CodexTelemetryTimingDebug:           runtimeCfg.CodexTelemetryTimingDebug,` (lines ~10229, ~11721 is the persist literal — skip it here, ~12009/12073 response literals), add directly after it:

```go
		CodexTurnStateStrict:                runtimeCfg.CodexTurnStateStrict,
		CodexSessionNoBorrowEnabled:         h.store.SessionNoBorrowEnabled(),
		CodexSessionNoBorrowHoldSeconds:     int(h.store.SessionNoBorrowHold() / time.Second),
		CodexInitialSessionAdmissionEnabled: runtimeCfg.CodexInitialSessionAdmissionEnabled,
		CodexInitialSessionMaxAgeSeconds:    runtimeCfg.CodexInitialSessionMaxAgeSeconds,
```

(`runtimeCfg` is the variable name used in each literal — in `GetSettings` it is obtained via `proxy.CurrentRuntimeSettings()`; use whatever local name that literal already uses for the telemetry line.)

- [ ] **Step 5: Apply runtime updates in UpdateSettings**

Directly after the block

```go
	if req.CodexTelemetryTimingDebug != nil {
		runtimeCfg.CodexTelemetryTimingDebug = *req.CodexTelemetryTimingDebug
		log.Printf("设置已更新: codex_telemetry_timing_debug = %t", runtimeCfg.CodexTelemetryTimingDebug)
	}
```
add:

```go
	if req.CodexTurnStateStrict != nil {
		runtimeCfg.CodexTurnStateStrict = *req.CodexTurnStateStrict
		log.Printf("设置已更新: codex_turn_state_strict = %t", runtimeCfg.CodexTurnStateStrict)
	}
	if req.CodexInitialSessionAdmissionEnabled != nil {
		runtimeCfg.CodexInitialSessionAdmissionEnabled = *req.CodexInitialSessionAdmissionEnabled
		log.Printf("设置已更新: codex_initial_session_admission_enabled = %t", runtimeCfg.CodexInitialSessionAdmissionEnabled)
	}
	if req.CodexInitialSessionMaxAgeSeconds != nil {
		runtimeCfg.CodexInitialSessionMaxAgeSeconds = database.NormalizeCodexInitialSessionMaxAgeSeconds(*req.CodexInitialSessionMaxAgeSeconds)
		log.Printf("设置已更新: codex_initial_session_max_age_seconds = %d", runtimeCfg.CodexInitialSessionMaxAgeSeconds)
	}
```

- [ ] **Step 6: Store-level no-borrow: fallback, override, persist, hot-apply**

Directly after `sessionSlotBufferSeconds := database.NormalizeSessionSlotBufferSeconds(int(h.store.GetSessionSlotBuffer() / time.Second))` (line ~10546) add:

```go
	sessionNoBorrowEnabled := h.store.SessionNoBorrowEnabled()
	sessionNoBorrowHoldSeconds := database.NormalizeSessionNoBorrowHoldSeconds(int(h.store.SessionNoBorrowHold() / time.Second))
```

Inside `if existingSettings != nil {`, directly after `sessionSlotBufferSeconds = database.NormalizeSessionSlotBufferSeconds(existingSettings.SessionSlotBufferSeconds)` add:

```go
		sessionNoBorrowEnabled = existingSettings.CodexSessionNoBorrowEnabled
		sessionNoBorrowHoldSeconds = database.NormalizeSessionNoBorrowHoldSeconds(existingSettings.CodexSessionNoBorrowHoldSeconds)
```

Directly after the block `if req.SessionSlotBufferSeconds != nil { sessionSlotBufferSeconds = ... }` add:

```go
	if req.CodexSessionNoBorrowEnabled != nil {
		sessionNoBorrowEnabled = *req.CodexSessionNoBorrowEnabled
	}
	if req.CodexSessionNoBorrowHoldSeconds != nil {
		sessionNoBorrowHoldSeconds = database.NormalizeSessionNoBorrowHoldSeconds(*req.CodexSessionNoBorrowHoldSeconds)
	}
```

In the persist literal (`h.db.UpdateSystemSettings(c.Request.Context(), &database.SystemSettings{`), directly after `CodexTelemetryTimingDebug:           runtimeCfg.CodexTelemetryTimingDebug,` (line ~11721) add:

```go
		CodexTurnStateStrict:                runtimeCfg.CodexTurnStateStrict,
		CodexSessionNoBorrowEnabled:         sessionNoBorrowEnabled,
		CodexSessionNoBorrowHoldSeconds:     sessionNoBorrowHoldSeconds,
		CodexInitialSessionAdmissionEnabled: runtimeCfg.CodexInitialSessionAdmissionEnabled,
		CodexInitialSessionMaxAgeSeconds:    runtimeCfg.CodexInitialSessionMaxAgeSeconds,
```

In the hot-apply `else` branch, directly after

```go
		if req.SessionSlotBufferEnabled != nil {
			h.store.SetSessionSlotBufferEnabled(sessionSlotBufferEnabled)
			log.Printf("设置已更新: session_slot_buffer_enabled = %t", sessionSlotBufferEnabled)
		}
```
add:

```go
		if req.CodexSessionNoBorrowEnabled != nil || req.CodexSessionNoBorrowHoldSeconds != nil {
			h.store.SetSessionNoBorrow(sessionNoBorrowEnabled, time.Duration(sessionNoBorrowHoldSeconds)*time.Second)
			log.Printf("设置已更新: codex_session_no_borrow_enabled = %t, hold = %ds", sessionNoBorrowEnabled, sessionNoBorrowHoldSeconds)
		}
```

Also in the persist-failure branch right above it (the `if req.SessionSlotBufferEnabled != nil || req.SessionSlotBufferSeconds != nil { writeError(...) return }` block), add a sibling check so a failed write does not silently drop the request:

```go
		if req.CodexSessionNoBorrowEnabled != nil || req.CodexSessionNoBorrowHoldSeconds != nil {
			writeError(c, http.StatusInternalServerError, "保存不借用设置失败，设置未生效")
			return
		}
```

- [ ] **Step 7: Run the admin test and the existing settings tests**

Run: `go build ./... && go test ./admin/ -run 'TestSessionGuardSettingsRoundtrip|TestUpdateSettings|TestGetSettings' -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

Run `gitnexus_detect_changes()`, then:

```bash
git add admin/handler.go admin/session_guard_settings_test.go
git commit -m "feat(session-guards): expose guard settings on the admin settings API"
```

---

### Task 3: Frontend settings UI + i18n + source guard

**Files:**
- Modify: `frontend/src/types.ts:2079` (after `session_slot_buffer_seconds: number`)
- Modify: `frontend/src/pages/Settings.tsx` (normalized defaults near line 2149; form defaults near line 2231; controls after the slot-buffer `SettingField` near line 5512)
- Modify: `frontend/src/locales/zh.json`, `frontend/src/locales/en.json`, `frontend/src/locales/zh-TW.json` (`settings` block, after `sessionSlotBufferSecondsDesc`)
- Test: `frontend/src/lib/sessionGuards.test.mjs`

**Interfaces:**
- Consumes: the five JSON keys from Task 2.

- [ ] **Step 1: Read `DESIGN.md` at the repo root** (required by CLAUDE.md before touching `.tsx`). Note the spacing/typography rules that apply to `SettingField`.

- [ ] **Step 2: Write the failing source-guard test**

Create `frontend/src/lib/sessionGuards.test.mjs`:

```js
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const settingsSource = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const locales = Object.fromEntries(
  ['zh', 'en', 'zh-TW'].map((name) => [name, JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), 'utf8'))]),
)

const SETTING_KEYS = [
  'codex_turn_state_strict',
  'codex_session_no_borrow_enabled',
  'codex_session_no_borrow_hold_seconds',
  'codex_initial_session_admission_enabled',
  'codex_initial_session_max_age_seconds',
]

const I18N_KEYS = [
  'codexTurnStateStrict', 'codexTurnStateStrictDesc',
  'codexSessionNoBorrow', 'codexSessionNoBorrowDesc',
  'codexSessionNoBorrowHoldSeconds', 'codexSessionNoBorrowHoldSecondsDesc',
  'codexInitialSessionAdmission', 'codexInitialSessionAdmissionDesc',
  'codexInitialSessionMaxAge', 'codexInitialSessionMaxAgeDesc',
]

test('session guard settings are typed, defaulted and rendered with shared controls', () => {
  for (const key of SETTING_KEYS) {
    assert.ok(typesSource.includes(`${key}:`) || typesSource.includes(`${key}?:`), `types.ts lacks ${key}`)
    assert.ok(settingsSource.includes(`${key}:`), `Settings.tsx form defaults lack ${key}`)
  }
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_turn_state_strict'"), 'turn-state strict switch missing')
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_session_no_borrow_enabled'"), 'no-borrow switch missing')
  assert.ok(settingsSource.includes('autoSaveSettingsPatch({ codex_session_no_borrow_hold_seconds: value })'), 'no-borrow hold input missing')
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_initial_session_admission_enabled'"), 'admission switch missing')
  assert.ok(settingsSource.includes('autoSaveSettingsPatch({ codex_initial_session_max_age_seconds: value })'), 'max age input missing')
  assert.equal(/<select[\s>]/.test(settingsSource.slice(settingsSource.indexOf('codex_turn_state_strict'))), false, 'no hand-written <select>')
})

test('session guard copy exists in every locale', () => {
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of I18N_KEYS) {
      assert.equal(typeof locale.settings?.[key], 'string', `${name}.json settings.${key} missing`)
    }
  }
})
```

- [ ] **Step 3: Run it to verify it fails**

Run: `cd frontend && node --experimental-strip-types --test src/lib/sessionGuards.test.mjs`
Expected: FAIL — `types.ts lacks codex_turn_state_strict`.

- [ ] **Step 4: Add the type fields**

In `frontend/src/types.ts` directly after `  session_slot_buffer_seconds: number` add:

```ts
  codex_turn_state_strict?: boolean
  codex_session_no_borrow_enabled?: boolean
  codex_session_no_borrow_hold_seconds?: number
  codex_initial_session_admission_enabled?: boolean
  codex_initial_session_max_age_seconds?: number
```

- [ ] **Step 5: Add defaults in Settings.tsx**

In `normalizeLazySettingsForm`, directly after `codex_telemetry_timing_debug: cacheNormalized.codex_telemetry_timing_debug ?? false,` add:

```ts
      codex_turn_state_strict: cacheNormalized.codex_turn_state_strict ?? false,
      codex_session_no_borrow_enabled: cacheNormalized.codex_session_no_borrow_enabled ?? false,
      codex_session_no_borrow_hold_seconds: cacheNormalized.codex_session_no_borrow_hold_seconds ?? 20,
      codex_initial_session_admission_enabled: cacheNormalized.codex_initial_session_admission_enabled ?? false,
      codex_initial_session_max_age_seconds: cacheNormalized.codex_initial_session_max_age_seconds ?? 180,
```

In the form defaults object, directly after `session_slot_buffer_seconds: 10,` add:

```ts
    codex_turn_state_strict: false,
    codex_session_no_borrow_enabled: false,
    codex_session_no_borrow_hold_seconds: 20,
    codex_initial_session_admission_enabled: false,
    codex_initial_session_max_age_seconds: 180,
```

- [ ] **Step 6: Add the controls**

Directly after the slot-buffer seconds `</SettingField>` (the one containing `autoSaveSettingsPatch({ session_slot_buffer_seconds: value })`, before the closing `</div>` of that grid) add:

```tsx
                    <SettingField label={t('settings.codexTurnStateStrict')} description={t('settings.codexTurnStateStrictDesc')} layout="switch" channels={CHANNELS_CODEX_ONLY}>
                      <Switch
                        checked={settingsForm.codex_turn_state_strict}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_turn_state_strict', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexSessionNoBorrow')} description={t('settings.codexSessionNoBorrowDesc')} layout="switch" channels={CHANNELS_CODEX_ONLY}>
                      <Switch
                        checked={settingsForm.codex_session_no_borrow_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_session_no_borrow_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexSessionNoBorrowHoldSeconds')}
                      description={t('settings.codexSessionNoBorrowHoldSecondsDesc')}
                      className={cn(!settingsForm.codex_session_no_borrow_enabled && 'opacity-60')}
                      channels={CHANNELS_CODEX_ONLY}
                    >
                      <DraftNumberInput
                        min={1}
                        max={30}
                        disabled={!settingsForm.codex_session_no_borrow_enabled}
                        value={settingsForm.codex_session_no_borrow_hold_seconds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_session_no_borrow_hold_seconds: value }))}
                        onValueCommit={(value) => void autoSaveSettingsPatch({ codex_session_no_borrow_hold_seconds: value })}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexInitialSessionAdmission')} description={t('settings.codexInitialSessionAdmissionDesc')} layout="switch" channels={CHANNELS_CODEX_ONLY}>
                      <Switch
                        checked={settingsForm.codex_initial_session_admission_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_initial_session_admission_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexInitialSessionMaxAge')}
                      description={t('settings.codexInitialSessionMaxAgeDesc')}
                      className={cn(!settingsForm.codex_initial_session_admission_enabled && 'opacity-60')}
                      channels={CHANNELS_CODEX_ONLY}
                    >
                      <DraftNumberInput
                        min={1}
                        max={86400}
                        disabled={!settingsForm.codex_initial_session_admission_enabled}
                        value={settingsForm.codex_initial_session_max_age_seconds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_initial_session_max_age_seconds: value }))}
                        onValueCommit={(value) => void autoSaveSettingsPatch({ codex_initial_session_max_age_seconds: value })}
                      />
                    </SettingField>
```

If `CHANNELS_CODEX_ONLY` is not in scope at that point of `Settings.tsx`, check how the neighbouring Codex-only fields around line 4141 declare it and reuse the same identifier; if `SettingField` in this card does not accept `channels`, drop the prop (keep everything else).

- [ ] **Step 7: Add i18n strings**

In `frontend/src/locales/zh.json` `settings` block, directly after the `"sessionSlotBufferSecondsDesc"` entry add:

```json
    "codexTurnStateStrict": "turn-state 严格模式",
    "codexTurnStateStrictDesc": "默认关闭。开启后，来源未知的 X-Codex-Turn-State 回带（绑定过期、重启、未记录）也一律剥离，并让上游 WebSocket 按帧携带 token 而不是固化在握手里。已知跨账号的回带无论开关都剥离。",
    "codexSessionNoBorrow": "会话不借用账号",
    "codexSessionNoBorrowDesc": "默认关闭。开启后，绑定账号并发满时先等待它空出来，等待期内不把这段会话借给其他账号；超过等待时间后恢复原有借用逻辑，避免硬失败。",
    "codexSessionNoBorrowHoldSeconds": "不借用等待时间（秒）",
    "codexSessionNoBorrowHoldSecondsDesc": "范围 1–30，默认 20。调度等待上限是 30 秒，设为 30 时不会再借用、等待超时直接返回无可用账号；建议 ≤ 25。",
    "codexInitialSessionAdmission": "首次会话 ID 年龄准入",
    "codexInitialSessionAdmissionDesc": "默认关闭。仅对 Codex 原生客户端、且亲和键没有现有账号绑定的会话生效：解析会话 UUIDv7 的时间戳，超过最大年龄或明显来自未来则拒绝（400，retry: stop）。非 v7 的 ID 只计数不拒绝。注意：粘性绑定 TTL 为 1 小时，未配 Redis 时重启会丢失绑定，闲置超过 1 小时的旧会话会被拒；请只在接受该代价时开启。",
    "codexInitialSessionMaxAge": "首次会话 ID 最大年龄（秒）",
    "codexInitialSessionMaxAgeDesc": "范围 1–86400，默认 180。年龄按网关收到请求的时刻减去会话 ID 内置时间戳计算；未来时间容忍 30 秒。",
```

In `frontend/src/locales/en.json` at the same position add:

```json
    "codexTurnStateStrict": "Strict turn-state mode",
    "codexTurnStateStrictDesc": "Off by default. When on, echoed X-Codex-Turn-State tokens of unknown origin (expired binding, restart, never recorded) are stripped too, and upstream WebSocket frames carry the token per frame instead of freezing it in the handshake. Known cross-account echoes are always stripped.",
    "codexSessionNoBorrow": "Do not borrow accounts for bound sessions",
    "codexSessionNoBorrowDesc": "Off by default. When on, a session whose bound account is at concurrency waits for that account instead of borrowing another one; after the hold expires the original spillover logic resumes so requests never hard-fail.",
    "codexSessionNoBorrowHoldSeconds": "No-borrow hold (seconds)",
    "codexSessionNoBorrowHoldSecondsDesc": "1–30, default 20. The scheduler wait budget is 30 s; at 30 no borrowing ever happens and a timed-out wait returns no available account. Keep it ≤ 25.",
    "codexInitialSessionAdmission": "Initial session ID age admission",
    "codexInitialSessionAdmissionDesc": "Off by default. Applies only to native Codex clients whose affinity key has no account binding yet: the session UUIDv7 timestamp is parsed and requests older than the max age or clearly from the future are rejected (400, retry: stop). Non-v7 IDs are counted, never rejected. Session affinity expires after 1 h and is lost on restart without Redis, so idle sessions older than that will be rejected — enable only if you accept that.",
    "codexInitialSessionMaxAge": "Initial session ID max age (seconds)",
    "codexInitialSessionMaxAgeDesc": "1–86400, default 180. Age = gateway receive time − timestamp embedded in the session ID; up to 30 s of future skew is tolerated.",
```

In `frontend/src/locales/zh-TW.json` directly after its `"sessionSlotBufferSeconds"` entry (line ~494) add the Traditional Chinese copy:

```json
    "codexTurnStateStrict": "turn-state 嚴格模式",
    "codexTurnStateStrictDesc": "預設關閉。開啟後，來源未知的 X-Codex-Turn-State 回帶（綁定過期、重啟、未記錄）也一律剝離，並讓上游 WebSocket 逐幀攜帶 token 而不是固化在握手中。已知跨帳號的回帶無論開關都會剝離。",
    "codexSessionNoBorrow": "工作階段不借用帳號",
    "codexSessionNoBorrowDesc": "預設關閉。開啟後，綁定帳號並發已滿時先等待它釋放，等待期間不把這段工作階段借給其他帳號；超過等待時間後恢復原有借用邏輯，避免硬性失敗。",
    "codexSessionNoBorrowHoldSeconds": "不借用等待時間（秒）",
    "codexSessionNoBorrowHoldSecondsDesc": "範圍 1–30，預設 20。排程等待上限為 30 秒，設為 30 時不會再借用、等待逾時直接回傳無可用帳號；建議 ≤ 25。",
    "codexInitialSessionAdmission": "首次工作階段 ID 年齡准入",
    "codexInitialSessionAdmissionDesc": "預設關閉。僅對 Codex 原生用戶端、且親和鍵沒有現有帳號綁定的工作階段生效：解析工作階段 UUIDv7 的時間戳，超過最大年齡或明顯來自未來則拒絕（400，retry: stop）。非 v7 的 ID 只計數不拒絕。注意：黏性綁定 TTL 為 1 小時，未設定 Redis 時重啟會遺失綁定，閒置超過 1 小時的舊工作階段會被拒；請只在接受此代價時開啟。",
    "codexInitialSessionMaxAge": "首次工作階段 ID 最大年齡（秒）",
    "codexInitialSessionMaxAgeDesc": "範圍 1–86400，預設 180。年齡以閘道收到請求的時刻減去工作階段 ID 內建時間戳計算；未來時間容忍 30 秒。",
```

- [ ] **Step 8: Run the guard test, the whole frontend suite, and typecheck**

Run: `cd frontend && npm test && npm run typecheck`
Expected: all tests PASS (including `sessionGuards.test.mjs`), `tsc` clean.

- [ ] **Step 9: Commit**

```bash
git add frontend/src/types.ts frontend/src/pages/Settings.tsx frontend/src/locales/zh.json frontend/src/locales/en.json frontend/src/locales/zh-TW.json frontend/src/lib/sessionGuards.test.mjs
git commit -m "feat(session-guards): settings UI for turn-state, no-borrow and initial-session guards"
```

---

### Task 4: Turn-state classification, policy and counters (`proxy/session_guards.go`)

**Files:**
- Create: `proxy/session_guards.go`
- Test: `proxy/session_guards_test.go`

**Interfaces:**
- Consumes: `codexTurnStateOrigins` / `codexTurnStateOrigin` / `codexTurnStateHeader` (`proxy/codex_turn_state.go`, `proxy/handler.go:255`), `(*auth.Store).SessionAffinityAccountID`, `CurrentRuntimeSettings().CodexTurnStateStrict`.
- Produces:
  - `type turnStateEchoClass string` with constants `turnStateEchoNone`, `turnStateEchoSame`, `turnStateEchoCross`, `turnStateEchoUnknown`
  - `func (h *Handler) applyCodexTurnStateEchoPolicy(affinityKey string, account *auth.Account, headers http.Header, body []byte) ([]byte, turnStateEchoClass, bool)` — mutates `headers` in place (deletes the header when stripping), returns possibly-modified body, the class, and whether anything was stripped
  - `func projectCodexTurnStateForWebsocket(body []byte, headers http.Header) ([]byte, http.Header)` — strict mode only: moves a remaining header token into `client_metadata.x-codex-turn-state` (if absent) and deletes the header; returns unchanged inputs when strict is off
  - `func recordTurnStateObservation(accountID int64, class turnStateEchoClass, stripped bool)` and the snapshot types used by Task 8: `type SessionGuardTurnStateCounters struct { Same, Cross, Unknown, Stripped uint64 }`, `type SessionGuardTurnStateAccount struct { AccountID int64; Counters SessionGuardTurnStateCounters }`, `func sessionGuardTurnStateSnapshot() (SessionGuardTurnStateCounters, []SessionGuardTurnStateAccount)` (accounts sorted by Cross+Unknown desc, max 20)
  - `func resetSessionGuardStatsForTest()`

- [ ] **Step 1: Write the failing tests**

Create `proxy/session_guards_test.go`:

```go
package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func newSessionGuardTestHandler(t *testing.T, accounts ...*auth.Account) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	for _, account := range accounts {
		store.AddAccountForTest(account)
	}
	return &Handler{store: store}
}

func setStrictTurnState(t *testing.T, strict bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexTurnStateStrict = strict; return s })
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

func TestApplyCodexTurnStateEchoPolicyClassifiesByExactOrigin(t *testing.T) {
	resetSessionGuardStatsForTest()
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	h := newSessionGuardTestHandler(t, minter, other)
	key := "guard-origin::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	noteCodexTurnStateProvenance(key, minter)

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoSame || stripped || headers.Get(codexTurnStateHeader) != "blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "blob" {
		t.Fatalf("same-account echo altered: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}

	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob","thread_id":"t"}}`))
	if class != turnStateEchoCross || !stripped || headers.Get(codexTurnStateHeader) != "" {
		t.Fatalf("cross-account header not stripped: class=%s stripped=%v header=%q", class, stripped, headers.Get(codexTurnStateHeader))
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() || gjson.GetBytes(body, "client_metadata.thread_id").String() != "t" {
		t.Fatalf("cross-account body token must be removed and siblings kept: %s", body)
	}
	totals, accounts := sessionGuardTurnStateSnapshot()
	if totals.Same != 1 || totals.Cross != 1 || totals.Stripped != 1 || len(accounts) != 2 {
		t.Fatalf("counters = %+v accounts=%d", totals, len(accounts))
	}
}

func TestApplyCodexTurnStateEchoPolicyFallsBackToBindingThenUnknown(t *testing.T) {
	resetSessionGuardStatsForTest()
	bound := &auth.Account{DBID: 301, AccessToken: "tok"}
	other := &auth.Account{DBID: 302, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, bound, other)
	key := "guard-binding::api-key:9"
	h.store.BindSessionAffinity(key, bound, "")

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, _ := h.applyCodexTurnStateEchoPolicy(key, bound, headers, []byte(`{}`)); class != turnStateEchoSame {
		t.Fatalf("binding-backed same classification = %s", class)
	}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, stripped := h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{}`)); class != turnStateEchoCross || !stripped {
		t.Fatalf("binding-backed cross classification = %s stripped=%v", class, stripped)
	}

	setStrictTurnState(t, false)
	unknownKey := "guard-unknown::api-key:9"
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(unknownKey, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoUnknown || stripped || headers.Get(codexTurnStateHeader) != "blob" || !gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("legacy mode must pass unknown echoes through: class=%s stripped=%v", class, stripped)
	}

	setStrictTurnState(t, true)
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(unknownKey, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoUnknown || !stripped || headers.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("strict mode must strip unknown echoes: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}

	if _, class, _ := h.applyCodexTurnStateEchoPolicy(unknownKey, other, http.Header{}, []byte(`{}`)); class != turnStateEchoNone {
		t.Fatalf("no token must classify as none: %s", class)
	}
	if _, class, _ := h.applyCodexTurnStateEchoPolicy("", other, headers, []byte(`{}`)); class != turnStateEchoNone {
		t.Fatalf("empty affinity key must not be tracked: %s", class)
	}
}

func TestApplyCodexTurnStateEchoPolicyIgnoresExpiredOrigin(t *testing.T) {
	resetSessionGuardStatsForTest()
	setStrictTurnState(t, false)
	other := &auth.Account{DBID: 402}
	h := newSessionGuardTestHandler(t, other)
	key := "guard-expired::api-key:9"
	codexTurnStateOrigins.Store(key, codexTurnStateOrigin{accountID: 401, expiresAt: time.Now().Add(-time.Minute)})
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, _ := h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{}`)); class != turnStateEchoUnknown {
		t.Fatalf("expired origin must fall through to unknown, got %s", class)
	}
}

func TestProjectCodexTurnStateForWebsocket(t *testing.T) {
	setStrictTurnState(t, false)
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	body, out := projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("legacy mode must keep the handshake header: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}

	setStrictTurnState(t, true)
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "blob" {
		t.Fatalf("strict mode must move the token into the frame: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
	if headers.Get(codexTurnStateHeader) != "blob" {
		t.Fatal("caller headers must not be mutated")
	}
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"client_metadata":{"x-codex-turn-state":"frame"}}`), headers)
	if out.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "frame" {
		t.Fatalf("existing frame token must win: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
}
```

If `auth.Store` has no `AddAccountForTest`, look in `auth/*_test.go` for how proxy tests inject accounts (e.g. `store.SetAccounts([]*auth.Account{...})` or constructing `&auth.Store{accounts: ...}` is not possible from another package). Search `proxy/handler_test.go` for `auth.NewStore(` usages that then add accounts and reuse that exact API in `newSessionGuardTestHandler`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./proxy/ -run 'TestApplyCodexTurnStateEchoPolicy|TestProjectCodexTurnStateForWebsocket' -count=1`
Expected: compile error — `applyCodexTurnStateEchoPolicy undefined`.

- [ ] **Step 3: Implement `proxy/session_guards.go`**

```go
package proxy

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 会话防护：把「别的账号铸造的 turn-state 被回带到当前账号」这类跨账号信号
// 分类、计数并按策略剥离。分类只回答一个问题——这个 token 是谁铸造的：
//   same    溯源账号 == 本次选中的账号
//   cross   溯源账号 != 本次账号（真实 Codex 永远不会产生，一律剥离）
//   unknown 没有溯源（绑定过期/重启/另一实例/从未记录）；legacy 透传，strict 剥离
// 溯源顺序：HTTP 下发时记录的精确来源表 → 亲和键当前绑定账号（含跨进程缓存）→ unknown。

type turnStateEchoClass string

const (
	turnStateEchoNone    turnStateEchoClass = "none"
	turnStateEchoSame    turnStateEchoClass = "same"
	turnStateEchoCross   turnStateEchoClass = "cross"
	turnStateEchoUnknown turnStateEchoClass = "unknown"
)

const codexTurnStateBodyPath = "client_metadata.x-codex-turn-state"

type SessionGuardTurnStateCounters struct {
	Same     uint64 `json:"same"`
	Cross    uint64 `json:"cross"`
	Unknown  uint64 `json:"unknown"`
	Stripped uint64 `json:"stripped"`
}

type SessionGuardTurnStateAccount struct {
	AccountID int64                         `json:"account_id"`
	Counters  SessionGuardTurnStateCounters `json:"counters"`
}

type sessionGuardStatsState struct {
	mu       sync.Mutex
	started  time.Time
	totals   SessionGuardTurnStateCounters
	accounts map[int64]*SessionGuardTurnStateCounters
}

var sessionGuardStats = newSessionGuardStats()

func newSessionGuardStats() *sessionGuardStatsState {
	return &sessionGuardStatsState{started: time.Now().UTC(), accounts: make(map[int64]*SessionGuardTurnStateCounters)}
}

func resetSessionGuardStatsForTest() {
	fresh := newSessionGuardStats()
	sessionGuardStats.mu.Lock()
	sessionGuardStats.started = fresh.started
	sessionGuardStats.totals = SessionGuardTurnStateCounters{}
	sessionGuardStats.accounts = fresh.accounts
	sessionGuardStats.mu.Unlock()
	resetInitialSessionStatsForTest()
}

func (c *SessionGuardTurnStateCounters) add(class turnStateEchoClass, stripped bool) {
	switch class {
	case turnStateEchoSame:
		c.Same++
	case turnStateEchoCross:
		c.Cross++
	case turnStateEchoUnknown:
		c.Unknown++
	default:
		return
	}
	if stripped {
		c.Stripped++
	}
}

func recordTurnStateObservation(accountID int64, class turnStateEchoClass, stripped bool) {
	if class == turnStateEchoNone {
		return
	}
	s := sessionGuardStats
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals.add(class, stripped)
	if accountID > 0 {
		counters := s.accounts[accountID]
		if counters == nil {
			// 账号数有上界（账号池），map 不会无界增长。
			counters = &SessionGuardTurnStateCounters{}
			s.accounts[accountID] = counters
		}
		counters.add(class, stripped)
	}
}

func sessionGuardTurnStateSnapshot() (SessionGuardTurnStateCounters, []SessionGuardTurnStateAccount) {
	s := sessionGuardStats
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]SessionGuardTurnStateAccount, 0, len(s.accounts))
	for id, counters := range s.accounts {
		accounts = append(accounts, SessionGuardTurnStateAccount{AccountID: id, Counters: *counters})
	}
	sort.Slice(accounts, func(i, j int) bool {
		li := accounts[i].Counters.Cross + accounts[i].Counters.Unknown
		lj := accounts[j].Counters.Cross + accounts[j].Counters.Unknown
		if li != lj {
			return li > lj
		}
		return accounts[i].AccountID < accounts[j].AccountID
	})
	if len(accounts) > 20 {
		accounts = accounts[:20]
	}
	return s.totals, accounts
}

func sessionGuardStartedAt() time.Time {
	s := sessionGuardStats
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// classifyCodexTurnStateEcho 只分类，不改任何东西。
func (h *Handler) classifyCodexTurnStateEcho(affinityKey string, account *auth.Account) turnStateEchoClass {
	if account == nil || account.ID() <= 0 {
		return turnStateEchoUnknown
	}
	if raw, ok := codexTurnStateOrigins.Load(affinityKey); ok {
		if origin, ok := raw.(codexTurnStateOrigin); ok && (origin.expiresAt.IsZero() || time.Now().Before(origin.expiresAt)) {
			if origin.accountID == account.ID() {
				return turnStateEchoSame
			}
			return turnStateEchoCross
		}
	}
	if h != nil && h.store != nil {
		if bound, ok := h.store.SessionAffinityAccountID(affinityKey); ok && bound > 0 {
			if bound == account.ID() {
				return turnStateEchoSame
			}
			return turnStateEchoCross
		}
	}
	return turnStateEchoUnknown
}

// applyCodexTurnStateEchoPolicy 在选号之后、出站之前调用一次（HTTP 与下游 WS 两条
// 尝试循环都调）。cross 一律剥离头 + 体；unknown 仅 strict 剥离；same/none 不动。
// headers 原地修改（调用方传的是本次尝试的下游头副本），body 返回可能改写后的副本。
func (h *Handler) applyCodexTurnStateEchoPolicy(affinityKey string, account *auth.Account, headers http.Header, body []byte) ([]byte, turnStateEchoClass, bool) {
	affinityKey = strings.TrimSpace(affinityKey)
	token := ""
	if headers != nil {
		token = strings.TrimSpace(headers.Get(codexTurnStateHeader))
	}
	bodyToken := strings.TrimSpace(gjson.GetBytes(body, codexTurnStateBodyPath).String())
	if token == "" {
		token = bodyToken
	}
	if token == "" || affinityKey == "" {
		return body, turnStateEchoNone, false
	}
	class := h.classifyCodexTurnStateEcho(affinityKey, account)
	strip := class == turnStateEchoCross || (class == turnStateEchoUnknown && CurrentRuntimeSettings().CodexTurnStateStrict)
	stripped := false
	if strip {
		if headers != nil && headers.Get(codexTurnStateHeader) != "" {
			headers.Del(codexTurnStateHeader)
			stripped = true
		}
		if bodyToken != "" {
			if updated, err := sjson.DeleteBytes(body, codexTurnStateBodyPath); err == nil {
				body = updated
				stripped = true
			}
		}
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	recordTurnStateObservation(accountID, class, stripped)
	if class != turnStateEchoSame {
		log.Printf("[TURN-STATE] account=%d class=%s stripped=%t affinity=%x", accountID, class, stripped, hashRiskIdentity(affinityKey))
	}
	return body, class, stripped
}

// projectCodexTurnStateForWebsocket 严格模式下把仍留在出站头里的 token 挪进帧体
// （官方 WS v2 契约：token 走 response.create.client_metadata，握手头逐连接冻结、
// 不能承载逐轮状态）。帧体已有 token 时以帧体为准。legacy 模式原样返回。
func projectCodexTurnStateForWebsocket(body []byte, headers http.Header) ([]byte, http.Header) {
	if !CurrentRuntimeSettings().CodexTurnStateStrict || headers == nil {
		return body, headers
	}
	token := strings.TrimSpace(headers.Get(codexTurnStateHeader))
	if token == "" {
		return body, headers
	}
	out := headers.Clone()
	out.Del(codexTurnStateHeader)
	if gjson.GetBytes(body, codexTurnStateBodyPath).Exists() || !gjson.ValidBytes(body) {
		return body, out
	}
	updated, err := sjson.SetBytes(body, codexTurnStateBodyPath, token)
	if err != nil {
		return body, out
	}
	return updated, out
}
```

`hashRiskIdentity` already exists in the proxy package (used by `newapi_policy.go`); if its return type is not printable with `%x`, use `%s` with its result. `resetInitialSessionStatsForTest` is defined in Task 7 — for this task, add a temporary stub `func resetInitialSessionStatsForTest() {}` in `session_guards.go` and remove it in Task 7 when the real one lands.

- [ ] **Step 4: Run the tests**

Run: `go test ./proxy/ -run 'TestApplyCodexTurnStateEchoPolicy|TestProjectCodexTurnStateForWebsocket|TestGuardCodexTurnState' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add proxy/session_guards.go proxy/session_guards_test.go
git commit -m "feat(session-guards): classify, count and strip cross-account turn-state echoes"
```

---

### Task 5: Wire turn-state policy into both attempt loops, record provenance on success, project on WS

**Files:**
- Modify: `proxy/handler.go:4929` (replace `guardCodexTurnStateEcho(...)` call), `proxy/handler.go` success paths near lines 4445 and 5647 (add provenance note), `proxy/responses_ws.go` near line 748 (policy) and 1482 (provenance)
- Modify: `proxy/executor.go:598` (WS projection)
- Test: `proxy/session_guards_wiring_test.go`

**Interfaces:**
- Consumes: Task 4 functions; `noteCodexTurnStateProvenance(affinityKey string, account *auth.Account)` (`proxy/codex_turn_state.go`).

- [ ] **Step 1: Run impact analysis** — `gitnexus_impact({target: "guardCodexTurnStateEcho", direction: "upstream"})`, `gitnexus_impact({target: "ExecuteRequest", direction: "upstream"})`. Report blast radius; `ExecuteRequest` will be HIGH (many callers) — the change is additive (one extra transform inside the WS branch), proceed after reporting.

- [ ] **Step 2: Write the failing wiring test**

Create `proxy/session_guards_wiring_test.go`:

```go
package proxy

import (
	"os"
	"regexp"
	"testing"
)

// 源码守卫：两条尝试循环和 WS 出站都必须经过策略/投影，成功路径必须记录溯源。
func TestSessionGuardWiringPresent(t *testing.T) {
	handler, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.ReadFile("responses_ws.go")
	if err != nil {
		t.Fatal(err)
	}
	executor, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`\n\s*guardCodexTurnStateEcho\(affinityKey, account, downstreamHeaders\)`).Match(handler) {
		t.Fatal("handler.go still calls the legacy guard directly; use applyCodexTurnStateEchoPolicy")
	}
	if got := regexp.MustCompile(`applyCodexTurnStateEchoPolicy\(affinityKey, account, downstreamHeaders, upstreamBody\)`).FindAll(handler, -1); len(got) != 1 {
		t.Fatalf("handler.go policy call sites = %d, want 1", len(got))
	}
	if got := regexp.MustCompile(`applyCodexTurnStateEchoPolicy\(affinityKey, account, downstreamHeaders, upstreamBody\)`).FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go policy call sites = %d, want 1", len(got))
	}
	if got := regexp.MustCompile(`noteCodexTurnStateProvenance\(affinityKey, account\)`).FindAll(handler, -1); len(got) < 2 {
		t.Fatalf("handler.go provenance notes = %d, want >= 2", len(got))
	}
	if got := regexp.MustCompile(`noteCodexTurnStateProvenance\(affinityKey, account\)`).FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go provenance notes = %d, want 1", len(got))
	}
	if !regexp.MustCompile(`projectCodexTurnStateForWebsocket\(requestBody, headers\)`).Match(executor) {
		t.Fatal("executor.go WS branch must project the turn-state into the frame")
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./proxy/ -run TestSessionGuardWiringPresent -count=1`
Expected: FAIL — "handler.go still calls the legacy guard directly".

- [ ] **Step 4: Wire handler.go**

Replace at line ~4929

```go
		// 换号后剥离旧账号铸造的 turn-state 回带,防止跨账号矛盾信号打到上游。
		guardCodexTurnStateEcho(affinityKey, account, downstreamHeaders)
```
with
```go
		// 跨账号 turn-state 回带一律剥离（头 + 体）；来源未知的按 strict 开关处理，
		// 并计数到会话防护统计。见 session_guards.go。
		upstreamBody, _, _ = h.applyCodexTurnStateEchoPolicy(affinityKey, account, downstreamHeaders, upstreamBody)
```

At line ~4445, change

```go
				if continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
					h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
				}
```
to
```go
				if continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
					h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
					// 成功尝试的账号就是客户端下一轮会回带的 turn-state 铸造者（WS 上游
					// 没有响应头可转发，只能在这里记）。
					noteCodexTurnStateProvenance(affinityKey, account)
				}
```

At line ~5647, change

```go
		if !continuousRetryBuffersAttempts(continuousRetryPolicy) || continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
			h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
		}
```
to
```go
		if !continuousRetryBuffersAttempts(continuousRetryPolicy) || continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
			h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
			noteCodexTurnStateProvenance(affinityKey, account)
		}
```

- [ ] **Step 5: Wire responses_ws.go**

Directly after `serviceTier = EffectiveRequestedServiceTier(upstreamBody, effectiveModel, downstreamHeaders, attemptIdentity)` (line ~742) add:

```go
		// 下游 WS 的 token 在帧体 client_metadata 里，之前从未被守卫过；这里与 HTTP 路径共用同一策略。
		upstreamBody, _, _ = h.applyCodexTurnStateEchoPolicy(affinityKey, account, downstreamHeaders, upstreamBody)
```

At line ~1482 change

```go
	if continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
		h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
	}
```
to
```go
	if continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
		h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
		noteCodexTurnStateProvenance(affinityKey, account)
	}
```

- [ ] **Step 6: Wire executor.go**

In the WS branch, directly after `requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, true, responsesLite)` (line ~598) add:

```go
		// strict 模式：turn-state 只放当前帧，不固化到逐连接冻结的握手头里。
		requestBody, headers = projectCodexTurnStateForWebsocket(requestBody, headers)
```

- [ ] **Step 7: Run the wiring test plus the whole proxy package**

Run: `go build ./... && go test ./proxy/ -count=1`
Expected: PASS (the proxy package takes a few minutes; run it in the foreground and wait).

- [ ] **Step 8: Commit**

Run `gitnexus_detect_changes()`; expected affected flows are the Responses HTTP/WS request flows only. Then:

```bash
git add proxy/handler.go proxy/responses_ws.go proxy/executor.go proxy/session_guards_wiring_test.go
git commit -m "feat(session-guards): apply turn-state policy on HTTP and WS paths and record provenance on success"
```

---

### Task 6: No-borrow session affinity

**Files:**
- Modify: `auth/store.go` (`nextForSessionWithFilter` near line 6984 and its two spillover sites near lines 7051 and 7090; `waitForSessionAvailableWithFilter` loop near line 8356; new `SessionBorrowStats`)
- Test: `auth/session_no_borrow_test.go`

**Interfaces:**
- Consumes: Task 1 setters.
- Produces: `type SessionBorrowStats struct { Borrowed uint64 \`json:"borrowed"\`; Held uint64 \`json:"held"\` }`, `func (s *Store) SessionBorrowStats() SessionBorrowStats`, internal `nextForSessionWithFilterBorrow(key, apiKeyID, exclude, filter, preserveBinding, policy, allowBorrow bool)`.

- [ ] **Step 1: Run impact analysis** — `gitnexus_impact({target: "nextForSessionWithFilter", direction: "upstream"})` and `gitnexus_impact({target: "waitForSessionAvailableWithFilter", direction: "upstream"})`. Report; both are HIGH (all dispatch paths). Proceed: the signatures of every existing public method stay identical.

- [ ] **Step 2: Write the failing tests**

Create `auth/session_no_borrow_test.go`:

```go
package auth

import (
	"context"
	"testing"
	"time"
)

func TestNoBorrowHoldsInsteadOfSpilloverAtCapacity(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, 5*time.Second)
	store.bindSessionAffinity("no-borrow", bound, "")

	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %p, want bound %p", held, bound)
	}
	defer store.Release(held)

	selected, _, _ := store.NextForSessionWithDispatchGuard("no-borrow", 0, nil, nil, DispatchPolicyStandard)
	if selected != nil {
		store.Release(selected)
		t.Fatalf("no-borrow must not spill over, got account %d", selected.DBID)
	}
	if stats := store.SessionBorrowStats(); stats.Held != 1 || stats.Borrowed != 0 {
		t.Fatalf("stats = %+v, want held 1 borrowed 0", stats)
	}

	store.SetSessionNoBorrow(false, 5*time.Second)
	selected, _, guard := store.NextForSessionWithDispatchGuard("no-borrow", 0, nil, nil, DispatchPolicyStandard)
	if selected != fallback || !guard.PreservesExisting() {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("legacy spillover broken: selected=%v guard=%+v", selected, guard)
	}
	store.ReleaseForSessionWithGuard(selected, "no-borrow", guard)
	if stats := store.SessionBorrowStats(); stats.Held != 1 || stats.Borrowed != 1 {
		t.Fatalf("stats = %+v, want held 1 borrowed 1", stats)
	}
}

func TestNoBorrowUnboundSessionStillSelectsFreely(t *testing.T) {
	a := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{accounts: []*Account{a}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, 5*time.Second)
	selected, _, _ := store.NextForSessionWithDispatchGuard("fresh", 0, nil, nil, DispatchPolicyStandard)
	if selected != a {
		t.Fatalf("fresh session must still pick an account, got %v", selected)
	}
	store.Release(selected)
}

func TestNoBorrowWaitBorrowsAfterHold(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, time.Second)
	store.sessionNoBorrowHoldNS.Store(int64(150 * time.Millisecond)) // 测试用短 hold，绕过 1s 下限
	store.bindSessionAffinity("no-borrow-wait", bound, "")
	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %v", held)
	}
	defer store.Release(held)

	started := time.Now()
	selected, _, guard := store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "no-borrow-wait", 2*time.Second, 0, nil, nil, DispatchPolicyStandard)
	elapsed := time.Since(started)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("after hold the wait must borrow the fallback, got %v", selected)
	}
	store.ReleaseForSessionWithGuard(selected, "no-borrow-wait", guard)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("borrowed before the hold expired: %s", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("hold expiry was not honoured promptly: %s", elapsed)
	}
	if stats := store.SessionBorrowStats(); stats.Borrowed != 1 || stats.Held == 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestNoBorrowWaitReturnsBoundAccountWhenItFrees(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, 10*time.Second)
	store.bindSessionAffinity("no-borrow-free", bound, "")
	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %v", held)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		store.Release(held)
	}()
	selected, _, _ := store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "no-borrow-free", 3*time.Second, 0, nil, nil, DispatchPolicyStandard)
	if selected != bound {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("wait must return the bound account once it frees, got %v", selected)
	}
	store.Release(selected)
	if stats := store.SessionBorrowStats(); stats.Borrowed != 0 {
		t.Fatalf("no borrow expected, stats = %+v", stats)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./auth/ -run 'TestNoBorrow' -count=1`
Expected: compile error — `store.SessionBorrowStats undefined`.

- [ ] **Step 4: Add the stats type and thread `allowBorrow`**

In `auth/store.go` `Store` struct, directly after `sessionNoBorrowHoldNS         atomic.Int64` add:

```go
	sessionBorrowed               atomic.Uint64
	sessionBorrowHeld             atomic.Uint64
```

Directly after `func (s *Store) SessionNoBorrowHold() time.Duration { ... }` add:

```go
// SessionBorrowStats 进程内计数：Borrowed = 实际发生的容量溢出借用；Held = 因
// 不借用策略被扣住（返回 nil 进入等待）的次数。
type SessionBorrowStats struct {
	Borrowed uint64 `json:"borrowed"`
	Held     uint64 `json:"held"`
}

func (s *Store) SessionBorrowStats() SessionBorrowStats {
	if s == nil {
		return SessionBorrowStats{}
	}
	return SessionBorrowStats{Borrowed: s.sessionBorrowed.Load(), Held: s.sessionBorrowHeld.Load()}
}
```

Rename the existing implementation: change

```go
func (s *Store) nextForSessionWithFilter(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, preserveBinding bool, policy DispatchPolicy) (*Account, string, SessionAffinityGuard) {
	if s == nil {
```
to
```go
func (s *Store) nextForSessionWithFilter(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, preserveBinding bool, policy DispatchPolicy) (*Account, string, SessionAffinityGuard) {
	allowBorrow := true
	if s != nil && s.SessionNoBorrowEnabled() {
		allowBorrow = false
	}
	return s.nextForSessionWithFilterBorrow(key, apiKeyID, exclude, filter, preserveBinding, policy, allowBorrow)
}

// nextForSessionWithFilterBorrow 是带借用开关的实现。allowBorrow=false 时，绑定
// 账号并发满不再借用其他账号而是返回 nil，由等待循环决定何时放开。
func (s *Store) nextForSessionWithFilterBorrow(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, preserveBinding bool, policy DispatchPolicy, allowBorrow bool) (*Account, string, SessionAffinityGuard) {
	if s == nil {
```

At the first spillover site (line ~7047) change

```go
			if capacityFull {
				fallback := s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, policy)
				if fallback == nil {
					return nil, "", SessionAffinityGuard{}
				}
				log.Printf("会话粘性容量溢出: 绑定账号=%d 并发满,本请求借用账号=%d(该请求预期上游缓存未命中)", binding.accountID, fallback.DBID)
				return fallback, "", SessionAffinityGuard{preserveAccountID: binding.accountID}
			}
```
to
```go
			if capacityFull {
				if !allowBorrow {
					s.sessionBorrowHeld.Add(1)
					return nil, "", SessionAffinityGuard{}
				}
				fallback := s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, policy)
				if fallback == nil {
					return nil, "", SessionAffinityGuard{}
				}
				s.sessionBorrowed.Add(1)
				log.Printf("会话粘性容量溢出: 绑定账号=%d 并发满,本请求借用账号=%d(该请求预期上游缓存未命中)", binding.accountID, fallback.DBID)
				return fallback, "", SessionAffinityGuard{preserveAccountID: binding.accountID}
			}
```

Apply the identical `if !allowBorrow { s.sessionBorrowHeld.Add(1); return nil, "", SessionAffinityGuard{} }` + `s.sessionBorrowed.Add(1)` edit at the second spillover site (line ~7086, the cached-binding branch that logs the same message).

- [ ] **Step 5: Teach the wait loop about the hold**

In `waitForSessionAvailableWithFilter`, directly after `expires := time.Now().Add(timeout)` add:

```go
	waitStarted := time.Now()
	noBorrow := !preserveBinding && s.SessionNoBorrowEnabled()
	noBorrowHold := s.SessionNoBorrowHold()
```

Change the selection inside the `for {` loop from

```go
		if preserveBinding {
			acc, proxyURL = s.NextForContinuationWithDispatch(key, apiKeyID, exclude, filter, policy)
		} else {
			acc, proxyURL, guard = s.NextForSessionWithDispatchGuard(key, apiKeyID, exclude, filter, policy)
		}
```
to
```go
		if preserveBinding {
			acc, proxyURL = s.NextForContinuationWithDispatch(key, apiKeyID, exclude, filter, policy)
		} else if noBorrow {
			// hold 期内只认绑定账号；到期后放开借用，避免把等待超时变成硬失败。
			acc, proxyURL, guard = s.nextForSessionWithFilterBorrow(key, apiKeyID, exclude, filter, false, policy, time.Since(waitStarted) >= noBorrowHold)
		} else {
			acc, proxyURL, guard = s.NextForSessionWithDispatchGuard(key, apiKeyID, exclude, filter, policy)
		}
```

The loop's notification wait must also wake at hold expiry. Find the `select {` inside this loop that waits on `waiter` / `deadline.C` / `heartbeatC` / `ctx.Done()`; add a hold timer: directly after `noBorrowHold := s.SessionNoBorrowHold()` add

```go
	var holdC <-chan time.Time
	if noBorrow {
		holdTimer := time.NewTimer(noBorrowHold)
		defer holdTimer.Stop()
		holdC = holdTimer.C
	}
```

and add a case to that `select`:

```go
		case <-holdC:
			holdC = nil // 只触发一次，之后立即重试一次选号（此时 allowBorrow 已为 true）
			continue
```

If the select already has a default polling interval, keep it; the new case only guarantees the retry happens as soon as the hold expires.

- [ ] **Step 6: Run the new tests and the affinity/scheduler suites**

Run: `go build ./... && go test ./auth/ -run 'TestNoBorrow|TestSessionCapacitySpillover|TestWaitForSessionAvailable|TestNextForSession|TestBoundedAffinity' -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

Run `gitnexus_detect_changes()`; expected affected flows: session dispatch/wait. Then:

```bash
git add auth/store.go auth/session_no_borrow_test.go
git commit -m "feat(session-guards): hold bound sessions instead of borrowing accounts at capacity"
```

---

### Task 7: Initial-session UUIDv7 age admission

**Files:**
- Create: `proxy/initial_session_admission.go`
- Modify: `proxy/handler.go:3891` (after `turnHasBinding`), `proxy/responses_ws.go:443` (after `turnHasBinding`)
- Modify: `proxy/session_guards.go` (remove the temporary `resetInitialSessionStatsForTest` stub)
- Test: `proxy/initial_session_admission_test.go`

**Interfaces:**
- Consumes: `requestSessionIdentity` (`proxy/executor.go:1348`), `EvaluateEngineFingerprint`, `IsCodexOfficialClientByHeaders`, `CurrentRuntimeSettings()`.
- Produces:
  - `type initialSessionVerdict string` (`"allowed"`, `"expired"`, `"future"`, `"invalid"`)
  - `func evaluateInitialSessionAge(id string, received time.Time, maxAge time.Duration) (initialSessionVerdict, time.Duration)`
  - `func (h *Handler) checkInitialSessionAdmission(headers http.Header, body []byte, identity requestSessionIdentity, hasBinding bool, received time.Time) *api.APIError`
  - `type SessionGuardInitialSummary struct { Samples, Allowed, Expired, Future, Invalid uint64; MaxAgeMillis int64; AverageAgeMillis float64 }`, `func sessionGuardInitialSnapshot(now time.Time) (recentHour, sinceStart SessionGuardInitialSummary)`, `func resetInitialSessionStatsForTest()`

- [ ] **Step 1: Write the failing tests**

Create `proxy/initial_session_admission_test.go`:

```go
package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
)

func v7At(t *testing.T, at time.Time) string {
	t.Helper()
	u, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	ms := uint64(at.UnixMilli())
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	return u.String()
}

func setInitialAdmission(t *testing.T, enabled bool, maxAgeSeconds int) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexInitialSessionAdmissionEnabled = enabled
		s.CodexInitialSessionMaxAgeSeconds = maxAgeSeconds
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

func TestEvaluateInitialSessionAge(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	max := 180 * time.Second
	cases := []struct {
		name string
		id   string
		want initialSessionVerdict
	}{
		{"fresh", v7At(t, now.Add(-10*time.Second)), "allowed"},
		{"exactly max", v7At(t, now.Add(-max)), "allowed"},
		{"expired", v7At(t, now.Add(-max-time.Millisecond)), "expired"},
		{"slightly future within tolerance", v7At(t, now.Add(20*time.Second)), "allowed"},
		{"future beyond tolerance", v7At(t, now.Add(31*time.Second)), "future"},
		{"v4", uuid.NewString(), "invalid"},
		{"garbage", "not-a-uuid", "invalid"},
		{"empty", "", "invalid"},
	}
	for _, tc := range cases {
		if got, _ := evaluateInitialSessionAge(tc.id, now, max); got != tc.want {
			t.Errorf("%s: verdict = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func codexHeaders(sessionID string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", "codex_cli_rs/0.154.0 (Mac OS 26.5.2; arm64) xterm-256color")
	h.Set("Originator", "codex_cli_rs")
	h.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	h.Set("session_id", sessionID)
	return h
}

func TestCheckInitialSessionAdmission(t *testing.T) {
	resetSessionGuardStatsForTest()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	h := &Handler{store: store}
	now := time.Now()
	old := v7At(t, now.Add(-time.Hour))
	fresh := v7At(t, now.Add(-5*time.Second))
	body := []byte(`{"model":"gpt-5.5","input":[]}`)

	setInitialAdmission(t, false, 180)
	if err := h.checkInitialSessionAdmission(codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now); err != nil {
		t.Fatalf("disabled guard must allow: %v", err)
	}

	setInitialAdmission(t, true, 180)
	if err := h.checkInitialSessionAdmission(codexHeaders(fresh), body, resolveRequestSessionIdentity(codexHeaders(fresh), body), false, now); err != nil {
		t.Fatalf("fresh session must be allowed: %v", err)
	}
	err := h.checkInitialSessionAdmission(codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now)
	if err == nil || string(err.Code) != "codex_session_identity_unavailable" {
		t.Fatalf("old unbound session must be rejected, got %v", err)
	}
	details, _ := err.Details.(map[string]any)
	if details == nil || details["retry"] != "stop" {
		t.Fatalf("rejection must carry retry: stop, got %#v", err.Details)
	}
	if err := h.checkInitialSessionAdmission(codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), true, now); err != nil {
		t.Fatalf("bound session must never be age-checked: %v", err)
	}

	plain := http.Header{}
	plain.Set("User-Agent", "curl/8.0")
	plain.Set("X-Session-Id", old)
	if err := h.checkInitialSessionAdmission(plain, body, resolveRequestSessionIdentity(plain, body), false, now); err != nil {
		t.Fatalf("non-Codex clients must be skipped: %v", err)
	}
	v4 := codexHeaders(uuid.NewString())
	if err := h.checkInitialSessionAdmission(v4, body, resolveRequestSessionIdentity(v4, body), false, now); err != nil {
		t.Fatalf("non-v7 ids are counted but never rejected: %v", err)
	}

	_, since := sessionGuardInitialSnapshot(time.Now())
	if since.Samples != 3 || since.Allowed != 1 || since.Expired != 1 || since.Invalid != 1 {
		t.Fatalf("stats = %+v", since)
	}
	if since.MaxAgeMillis < 3_599_000 {
		t.Fatalf("max age must reflect the hour-old sample: %d", since.MaxAgeMillis)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./proxy/ -run 'TestEvaluateInitialSessionAge|TestCheckInitialSessionAdmission' -count=1`
Expected: compile error — `evaluateInitialSessionAge undefined`.

- [ ] **Step 3: Implement `proxy/initial_session_admission.go`**

```go
package proxy

import (
	"encoding/binary"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 首次会话准入：Codex 原生客户端、亲和键还没有账号绑定的会话，第一次选号前
// 用会话 ID（UUIDv7）内置的毫秒时间戳算年龄。太旧的 ID 在这里出现，要么是
// 客户端在重放别处创建的旧会话去抢绑定，要么是网关自己丢了绑定（TTL/重启）。
// 后者是运营代价（见 UI 说明），前者是本功能要挡的。非 v7 的 ID 只计数不拒绝：
// 威胁模型是重放真实旧 ID，真实 Codex ID 必然是 v7；伪造 v4 只会建一个新绑定。

type initialSessionVerdict string

const (
	initialSessionAllowed initialSessionVerdict = "allowed"
	initialSessionExpired initialSessionVerdict = "expired"
	initialSessionFuture  initialSessionVerdict = "future"
	initialSessionInvalid initialSessionVerdict = "invalid"

	// 客户端时钟略快是常态（尤其 Windows 桌面），30 秒内的"未来"视为合法。
	initialSessionFutureTolerance = 30 * time.Second
)

func evaluateInitialSessionAge(id string, received time.Time, maxAge time.Duration) (initialSessionVerdict, time.Duration) {
	u, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || u.Version() != 7 || u.Variant() != uuid.RFC4122 {
		return initialSessionInvalid, 0
	}
	var raw [8]byte
	copy(raw[2:], u[:6])
	idTime := time.UnixMilli(int64(binary.BigEndian.Uint64(raw[:])))
	age := received.Sub(idTime)
	switch {
	case age < -initialSessionFutureTolerance:
		return initialSessionFuture, age
	case age > maxAge:
		return initialSessionExpired, age
	default:
		return initialSessionAllowed, age
	}
}

type SessionGuardInitialSummary struct {
	Samples          uint64  `json:"samples"`
	Allowed          uint64  `json:"allowed"`
	Expired          uint64  `json:"expired"`
	Future           uint64  `json:"future"`
	Invalid          uint64  `json:"invalid"`
	MaxAgeMillis     int64   `json:"max_age_ms"`
	AverageAgeMillis float64 `json:"average_age_ms"`
	sumAgeMillis     float64
	validSamples     uint64
}

func (s *SessionGuardInitialSummary) add(verdict initialSessionVerdict, age time.Duration) {
	s.Samples++
	switch verdict {
	case initialSessionAllowed:
		s.Allowed++
	case initialSessionExpired:
		s.Expired++
	case initialSessionFuture:
		s.Future++
	default:
		s.Invalid++
		return
	}
	if verdict == initialSessionFuture {
		return
	}
	ms := age.Milliseconds()
	s.validSamples++
	s.sumAgeMillis += float64(ms)
	if ms > s.MaxAgeMillis {
		s.MaxAgeMillis = ms
	}
}

func (s *SessionGuardInitialSummary) merge(o SessionGuardInitialSummary) {
	s.Samples += o.Samples
	s.Allowed += o.Allowed
	s.Expired += o.Expired
	s.Future += o.Future
	s.Invalid += o.Invalid
	s.validSamples += o.validSamples
	s.sumAgeMillis += o.sumAgeMillis
	if o.MaxAgeMillis > s.MaxAgeMillis {
		s.MaxAgeMillis = o.MaxAgeMillis
	}
}

func (s *SessionGuardInitialSummary) finish() {
	if s.validSamples > 0 {
		s.AverageAgeMillis = s.sumAgeMillis / float64(s.validSamples)
	}
}

type initialSessionBucket struct {
	second  int64
	summary SessionGuardInitialSummary
}

type initialSessionStatsState struct {
	mu      sync.Mutex
	total   SessionGuardInitialSummary
	buckets [3600]initialSessionBucket
}

var initialSessionStats = &initialSessionStatsState{}

func resetInitialSessionStatsForTest() {
	initialSessionStats.mu.Lock()
	initialSessionStats.total = SessionGuardInitialSummary{}
	initialSessionStats.buckets = [3600]initialSessionBucket{}
	initialSessionStats.mu.Unlock()
}

func recordInitialSessionVerdict(received time.Time, verdict initialSessionVerdict, age time.Duration) {
	s := initialSessionStats
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total.add(verdict, age)
	second := received.Unix()
	b := &s.buckets[((second%3600)+3600)%3600]
	if b.second > second {
		return
	}
	if b.second != second {
		*b = initialSessionBucket{second: second}
	}
	b.summary.add(verdict, age)
}

func sessionGuardInitialSnapshot(now time.Time) (recentHour, sinceStart SessionGuardInitialSummary) {
	s := initialSessionStats
	s.mu.Lock()
	defer s.mu.Unlock()
	sinceStart = s.total
	floor := now.Unix() - 3600
	for _, b := range s.buckets {
		if b.second <= floor || b.second > now.Unix() {
			continue
		}
		recentHour.merge(b.summary)
	}
	recentHour.finish()
	sinceStart.finish()
	return recentHour, sinceStart
}

func isCodexNativeRequest(headers http.Header, body []byte) bool {
	if headers == nil {
		return false
	}
	if EvaluateEngineFingerprint(headers, body, nil) {
		return true
	}
	return IsCodexOfficialClientByHeaders(headers.Get("User-Agent"), headers.Get("Originator"))
}

func initialSessionAdmissionError() *api.APIError {
	return api.NewAPIErrorWithDetails(
		api.ErrorCode("codex_session_identity_unavailable"),
		"当前会话无法继续处理，请重新打开对话；仍失败时请新建对话。",
		api.ErrorTypeInvalidRequest,
		gin.H{"retry": "stop"},
	)
}

// checkInitialSessionAdmission 在 affinityKey / turnHasBinding 算出之后、第一次选号
// 之前调用。返回非 nil 即拒绝。hasBinding=true 或非 Codex 原生请求直接放行。
func (h *Handler) checkInitialSessionAdmission(headers http.Header, body []byte, identity requestSessionIdentity, hasBinding bool, received time.Time) *api.APIError {
	settings := CurrentRuntimeSettings()
	if !settings.CodexInitialSessionAdmissionEnabled || hasBinding {
		return nil
	}
	sessionID := strings.TrimSpace(identity.explicitUpstreamID)
	if sessionID == "" || !isCodexNativeRequest(headers, body) {
		return nil
	}
	maxAge := time.Duration(database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds)) * time.Second
	verdict, age := evaluateInitialSessionAge(sessionID, received, maxAge)
	recordInitialSessionVerdict(received, verdict, age)
	if verdict == initialSessionExpired || verdict == initialSessionFuture {
		log.Printf("[INITIAL-SESSION] rejected verdict=%s age=%s limit=%s session=%x", verdict, age.Round(time.Millisecond), maxAge, hashRiskIdentity(sessionID))
		return initialSessionAdmissionError()
	}
	return nil
}
```

Remove the temporary `func resetInitialSessionStatsForTest() {}` stub from `proxy/session_guards.go`. If `hashRiskIdentity` is not `%x`-printable, format it with `%s`.

- [ ] **Step 4: Run the unit tests**

Run: `go test ./proxy/ -run 'TestEvaluateInitialSessionAge|TestCheckInitialSessionAdmission|TestApplyCodexTurnStateEchoPolicy' -count=1`
Expected: PASS.

- [ ] **Step 5: Wire the two entry points**

In `proxy/handler.go`, directly after (line ~3892)

```go
	turnContinuationPinned := turnContinuation && turnHasBinding
```
add:

```go
	if failure := h.checkInitialSessionAdmission(c.Request.Header, rawBody, sessionIdentity, turnHasBinding, handlerStart); failure != nil {
		api.SendError(c, failure)
		return
	}
```

(`handlerStart` is the `time.Now()` captured at the top of `Responses`; if the variable in scope is named differently at that point, use that name — it must be the request receive time.)

In `proxy/responses_ws.go`, directly after (line ~443)

```go
	_, turnHasBinding := h.store.SessionAffinityAccountID(affinityKey)
```
add:

```go
	if failure := h.checkInitialSessionAdmission(c.Request.Header, rawBody, sessionIdentity, turnHasBinding, time.Now()); failure != nil {
		_ = writeResponsesWSError(conn, failure)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, failure.Message, failure)
	}
```

- [ ] **Step 6: Extend the wiring guard test**

Append to `TestSessionGuardWiringPresent` in `proxy/session_guards_wiring_test.go` (before its closing brace):

```go
	if got := regexp.MustCompile(`checkInitialSessionAdmission\(c\.Request\.Header, rawBody, sessionIdentity, turnHasBinding,`).FindAll(handler, -1); len(got) != 1 {
		t.Fatalf("handler.go admission call sites = %d, want 1", len(got))
	}
	if got := regexp.MustCompile(`checkInitialSessionAdmission\(c\.Request\.Header, rawBody, sessionIdentity, turnHasBinding,`).FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go admission call sites = %d, want 1", len(got))
	}
```

- [ ] **Step 7: Build and run the proxy package**

Run: `go build ./... && go test ./proxy/ -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

Run `gitnexus_detect_changes()`, then:

```bash
git add proxy/initial_session_admission.go proxy/initial_session_admission_test.go proxy/session_guards.go proxy/session_guards_wiring_test.go proxy/handler.go proxy/responses_ws.go
git commit -m "feat(session-guards): admit unbound Codex sessions by UUIDv7 age"
```

---

### Task 8: Runtime status panel (backend + frontend)

**Files:**
- Create: `proxy/session_guard_status.go`
- Modify: `admin/responses.go:268` (`runtimeStatusResponse`), `admin/runtime_status.go:88` (populate)
- Modify: `frontend/src/types.ts:1895` (`RuntimeStatusResponse`), `frontend/src/pages/RuntimeStatus.tsx` (new `StatusPanel` after the accounts panel), `frontend/src/locales/{zh,en,zh-TW}.json` (`runtime` block; zh-TW gets a new `runtime` block)
- Test: `admin/session_guard_status_test.go`, extend `frontend/src/lib/sessionGuards.test.mjs`

**Interfaces:**
- Consumes: Task 4 `sessionGuardTurnStateSnapshot`, `sessionGuardStartedAt`; Task 6 `(*auth.Store).SessionBorrowStats()`; Task 7 `sessionGuardInitialSnapshot`.
- Produces: `proxy.SessionGuardStatus` (JSON below) and `proxy.SessionGuardStatusSnapshot(store *auth.Store) SessionGuardStatus`; JSON key `session_guards` on `GET /api/admin/runtime`.

- [ ] **Step 1: Write the failing admin test**

Create `admin/session_guard_status_test.go`:

```go
package admin

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestRuntimeStatusIncludesSessionGuards(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	h := &Handler{store: store}
	status := h.buildRuntimeStatus(context.Background(), httptest.NewRequest("GET", "/api/admin/runtime", nil))
	guards := status.SessionGuards
	if guards.StartedAt == "" {
		t.Fatal("session_guards.started_at missing")
	}
	if guards.Settings.NoBorrowHoldSeconds != 20 || guards.Settings.InitialSessionMaxAgeSeconds != 180 {
		t.Fatalf("settings snapshot = %+v", guards.Settings)
	}
	if guards.TurnState.Accounts == nil || guards.Borrow.Borrowed != 0 || guards.InitialSession.SinceStart.Samples != 0 {
		t.Fatalf("fresh counters = %+v", guards)
	}
}
```

If `buildRuntimeStatus` panics on a nil `h.db` / `h.cache` in this minimal handler, look at how other tests in `admin/` call it (grep `buildRuntimeStatus` in `admin/*_test.go`) and reuse their handler construction instead of `&Handler{store: store}`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./admin/ -run TestRuntimeStatusIncludesSessionGuards -count=1`
Expected: compile error — `status.SessionGuards undefined`.

- [ ] **Step 3: Implement the snapshot in proxy**

Create `proxy/session_guard_status.go`:

```go
package proxy

import (
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

type SessionGuardSettingsStatus struct {
	TurnStateStrict                bool `json:"turn_state_strict"`
	NoBorrowEnabled                bool `json:"no_borrow_enabled"`
	NoBorrowHoldSeconds            int  `json:"no_borrow_hold_seconds"`
	InitialSessionAdmissionEnabled bool `json:"initial_session_admission_enabled"`
	InitialSessionMaxAgeSeconds    int  `json:"initial_session_max_age_seconds"`
}

type SessionGuardTurnStateStatus struct {
	Totals   SessionGuardTurnStateCounters  `json:"totals"`
	Accounts []SessionGuardTurnStateAccount `json:"accounts"`
}

type SessionGuardInitialStatus struct {
	RecentHour SessionGuardInitialSummary `json:"recent_hour"`
	SinceStart SessionGuardInitialSummary `json:"since_start"`
}

type SessionGuardStatus struct {
	StartedAt      string                      `json:"started_at"`
	Settings       SessionGuardSettingsStatus  `json:"settings"`
	TurnState      SessionGuardTurnStateStatus `json:"turn_state"`
	Borrow         auth.SessionBorrowStats     `json:"borrow"`
	InitialSession SessionGuardInitialStatus   `json:"initial_session"`
}

// SessionGuardStatusSnapshot 供 /api/admin/runtime 使用：全部是进程内计数，重启清零。
func SessionGuardStatusSnapshot(store *auth.Store) SessionGuardStatus {
	settings := CurrentRuntimeSettings()
	totals, accounts := sessionGuardTurnStateSnapshot()
	if accounts == nil {
		accounts = []SessionGuardTurnStateAccount{}
	}
	recent, since := sessionGuardInitialSnapshot(time.Now())
	status := SessionGuardStatus{
		StartedAt: sessionGuardStartedAt().Format(time.RFC3339),
		Settings: SessionGuardSettingsStatus{
			TurnStateStrict:                settings.CodexTurnStateStrict,
			InitialSessionAdmissionEnabled: settings.CodexInitialSessionAdmissionEnabled,
			InitialSessionMaxAgeSeconds:    database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds),
			NoBorrowHoldSeconds:            20,
		},
		TurnState:      SessionGuardTurnStateStatus{Totals: totals, Accounts: accounts},
		InitialSession: SessionGuardInitialStatus{RecentHour: recent, SinceStart: since},
	}
	if store != nil {
		status.Settings.NoBorrowEnabled = store.SessionNoBorrowEnabled()
		status.Settings.NoBorrowHoldSeconds = int(store.SessionNoBorrowHold() / time.Second)
		status.Borrow = store.SessionBorrowStats()
	}
	return status
}
```

- [ ] **Step 4: Expose it from admin**

In `admin/responses.go` `runtimeStatusResponse`, directly after `AdminAuth    runtimeAdminAuthResponse     \`json:"admin_auth"\`` add:

```go
	SessionGuards proxy.SessionGuardStatus     `json:"session_guards"`
```

(`admin/responses.go` must import `github.com/codex2api/proxy` — add it if the file does not already.)

In `admin/runtime_status.go` `buildRuntimeStatus`, in the returned literal directly after `AdminAuth:    adminAuth,` add:

```go
		SessionGuards: proxy.SessionGuardStatusSnapshot(h.store),
```

- [ ] **Step 5: Run the admin test**

Run: `go build ./... && go test ./admin/ -run 'TestRuntimeStatus' -count=1`
Expected: PASS.

- [ ] **Step 6: Frontend types**

In `frontend/src/types.ts` inside `RuntimeStatusResponse`, directly before `checks: RuntimeCheck[]` (find that line inside the interface) add:

```ts
  session_guards?: {
    started_at: ISODateString
    settings: {
      turn_state_strict: boolean
      no_borrow_enabled: boolean
      no_borrow_hold_seconds: number
      initial_session_admission_enabled: boolean
      initial_session_max_age_seconds: number
    }
    turn_state: {
      totals: { same: number; cross: number; unknown: number; stripped: number }
      accounts: Array<{ account_id: number; counters: { same: number; cross: number; unknown: number; stripped: number } }>
    }
    borrow: { borrowed: number; held: number }
    initial_session: {
      recent_hour: InitialSessionSummary
      since_start: InitialSessionSummary
    }
  }
```

and, directly above `export interface RuntimeStatusResponse {`, add:

```ts
export interface InitialSessionSummary {
  samples: number
  allowed: number
  expired: number
  future: number
  invalid: number
  max_age_ms: number
  average_age_ms: number
}
```

- [ ] **Step 7: Frontend panel**

In `frontend/src/pages/RuntimeStatus.tsx`, directly after the accounts `<StatusPanel ... />` (the one with `title={t('runtime.accounts')}`) add:

```tsx
              {status.session_guards && (
                <StatusPanel
                  title={t('runtime.sessionGuards')}
                  status={status.status}
                  icon={<ShieldCheck className="size-5" />}
                  rows={[
                    [t('runtime.sessionGuardsSwitches'), formatSessionGuardSwitches(status.session_guards.settings, t)],
                    [t('runtime.turnStateTotals'), formatTurnStateCounters(status.session_guards.turn_state.totals)],
                    [t('runtime.turnStateTopAccounts'), status.session_guards.turn_state.accounts.slice(0, 5).map((row) => `#${row.account_id} ${formatTurnStateCounters(row.counters)}`).join(' · ') || '-'],
                    [t('runtime.sessionBorrow'), `${t('runtime.borrowed')} ${formatNumber(status.session_guards.borrow.borrowed)} / ${t('runtime.held')} ${formatNumber(status.session_guards.borrow.held)}`],
                    [t('runtime.initialSessionRecentHour'), formatInitialSessionSummary(status.session_guards.initial_session.recent_hour, t)],
                    [t('runtime.initialSessionSinceStart'), formatInitialSessionSummary(status.session_guards.initial_session.since_start, t)],
                  ]}
                />
              )}
```

Add `ShieldCheck` to the existing `lucide-react` import list at the top of the file. Add these helpers next to the existing `formatStatusCounts` helper in the same file:

```tsx
function formatTurnStateCounters(c: { same: number; cross: number; unknown: number; stripped: number }): string {
  return `same ${formatNumber(c.same)} · cross ${formatNumber(c.cross)} · unknown ${formatNumber(c.unknown)} · stripped ${formatNumber(c.stripped)}`
}

function formatSessionGuardSwitches(
  s: { turn_state_strict: boolean; no_borrow_enabled: boolean; no_borrow_hold_seconds: number; initial_session_admission_enabled: boolean; initial_session_max_age_seconds: number },
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  const on = t('common.enabled')
  const off = t('common.disabled')
  return [
    `${t('runtime.turnStateStrict')}: ${s.turn_state_strict ? on : off}`,
    `${t('runtime.noBorrow')}: ${s.no_borrow_enabled ? `${on} (${s.no_borrow_hold_seconds}s)` : off}`,
    `${t('runtime.initialSessionAdmission')}: ${s.initial_session_admission_enabled ? `${on} (${s.initial_session_max_age_seconds}s)` : off}`,
  ].join(' · ')
}

function formatInitialSessionSummary(
  s: { samples: number; allowed: number; expired: number; future: number; invalid: number; max_age_ms: number; average_age_ms: number },
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  if (!s.samples) return t('runtime.noSamples')
  const avg = s.average_age_ms ? `${(s.average_age_ms / 1000).toFixed(1)}s` : '-'
  const max = s.max_age_ms ? `${(s.max_age_ms / 1000).toFixed(1)}s` : '-'
  return `${formatNumber(s.samples)} · ${t('runtime.allowed')} ${formatNumber(s.allowed)} · ${t('runtime.expired')} ${formatNumber(s.expired)} · ${t('runtime.future')} ${formatNumber(s.future)} · ${t('runtime.invalidId')} ${formatNumber(s.invalid)} · avg ${avg} · max ${max}`
}
```

Match the `t` parameter type to whatever `formatStatusCounts` already declares in that file (reuse its exact type alias if there is one).

- [ ] **Step 8: i18n for the panel**

In `frontend/src/locales/zh.json` `runtime` block (line ~2231) add, after its first key:

```json
    "sessionGuards": "会话防护",
    "sessionGuardsSwitches": "开关",
    "turnStateStrict": "turn-state 严格",
    "noBorrow": "不借用",
    "initialSessionAdmission": "首次会话准入",
    "turnStateTotals": "turn-state 回带分类",
    "turnStateTopAccounts": "外来 token 最多的账号",
    "sessionBorrow": "容量溢出借用",
    "borrowed": "已借用",
    "held": "已扣住",
    "initialSessionRecentHour": "首次会话（最近 1 小时）",
    "initialSessionSinceStart": "首次会话（启动以来）",
    "allowed": "放行",
    "expired": "超龄拒绝",
    "future": "未来时间拒绝",
    "invalidId": "非 v7",
    "noSamples": "暂无样本",
```

In `frontend/src/locales/en.json` `runtime` block (line ~2231) add the English:

```json
    "sessionGuards": "Session guards",
    "sessionGuardsSwitches": "Switches",
    "turnStateStrict": "strict turn-state",
    "noBorrow": "no-borrow",
    "initialSessionAdmission": "initial-session admission",
    "turnStateTotals": "Turn-state echo classes",
    "turnStateTopAccounts": "Accounts receiving foreign tokens",
    "sessionBorrow": "Capacity spillover",
    "borrowed": "borrowed",
    "held": "held",
    "initialSessionRecentHour": "Initial sessions (last hour)",
    "initialSessionSinceStart": "Initial sessions (since start)",
    "allowed": "allowed",
    "expired": "expired",
    "future": "future",
    "invalidId": "non-v7",
    "noSamples": "no samples yet",
```

In `frontend/src/locales/zh-TW.json` add a new top-level `"runtime"` block (place it directly before the top-level `"settings"` block, keeping valid JSON) with the Traditional Chinese copy:

```json
  "runtime": {
    "sessionGuards": "工作階段防護",
    "sessionGuardsSwitches": "開關",
    "turnStateStrict": "turn-state 嚴格",
    "noBorrow": "不借用",
    "initialSessionAdmission": "首次工作階段准入",
    "turnStateTotals": "turn-state 回帶分類",
    "turnStateTopAccounts": "外來 token 最多的帳號",
    "sessionBorrow": "容量溢出借用",
    "borrowed": "已借用",
    "held": "已扣住",
    "initialSessionRecentHour": "首次工作階段（最近 1 小時）",
    "initialSessionSinceStart": "首次工作階段（啟動以來）",
    "allowed": "放行",
    "expired": "超齡拒絕",
    "future": "未來時間拒絕",
    "invalidId": "非 v7",
    "noSamples": "暫無樣本"
  },
```

- [ ] **Step 9: Extend the frontend guard test**

Append to `frontend/src/lib/sessionGuards.test.mjs`:

```js
const runtimeSource = readFileSync(new URL('../pages/RuntimeStatus.tsx', import.meta.url), 'utf8')

test('runtime status renders the session guards panel with shared StatusPanel', () => {
  assert.ok(runtimeSource.includes("t('runtime.sessionGuards')"), 'panel title missing')
  assert.ok(runtimeSource.includes('status.session_guards.turn_state.totals'), 'turn-state totals row missing')
  assert.ok(runtimeSource.includes('status.session_guards.borrow.borrowed'), 'borrow row missing')
  assert.ok(runtimeSource.includes('status.session_guards.initial_session.recent_hour'), 'initial session row missing')
  assert.ok(typesSource.includes('session_guards?:'), 'RuntimeStatusResponse.session_guards missing')
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of ['sessionGuards', 'turnStateTotals', 'sessionBorrow', 'initialSessionRecentHour', 'noSamples']) {
      assert.equal(typeof locale.runtime?.[key], 'string', `${name}.json runtime.${key} missing`)
    }
  }
})
```

- [ ] **Step 10: Run the frontend suite and typecheck**

Run: `cd frontend && npm test && npm run typecheck`
Expected: PASS, `tsc` clean.

- [ ] **Step 11: Commit**

```bash
git add proxy/session_guard_status.go admin/responses.go admin/runtime_status.go admin/session_guard_status_test.go frontend/src/types.ts frontend/src/pages/RuntimeStatus.tsx frontend/src/locales/zh.json frontend/src/locales/en.json frontend/src/locales/zh-TW.json frontend/src/lib/sessionGuards.test.mjs
git commit -m "feat(session-guards): surface guard counters on the runtime status page"
```

---

### Task 9: Operator doc, full verification, final commit

**Files:**
- Create: `docs/session-guards.md`
- Modify: `docs/CONFIGURATION.md` (short pointer paragraph in the Codex section)

- [ ] **Step 1: Write the operator doc**

Create `docs/session-guards.md`:

```markdown
# 会话防护（Session Guards）

四项默认关闭的开关，用于验证「跨账号 turn-state / 上下文回带导致上游按身份降载」这一假设。全部热更新生效，进程内计数在「运维 → 运行状态 → 会话防护」查看，重启清零。

| 设置 | 默认 | 作用 |
|---|---|---|
| 无（始终采集） | — | 每个 Codex 请求的入站 `X-Codex-Turn-State`（头或 `client_metadata.x-codex-turn-state`）按 same / cross / unknown 分类计数；cross、unknown 打 `[TURN-STATE]` 日志 |
| turn-state 严格模式 | 关 | 关：只剥离已知跨账号的回带（头 + 体）。开：来源未知的也剥离；上游 WS 握手不再带 token，改放每帧 client_metadata |
| 会话不借用账号 + 等待秒数 | 关 / 20 | 绑定账号并发满时先等它空出来（最多等待秒数），期内不借用其他账号；到期恢复原逻辑 |
| 首次会话 ID 年龄准入 + 最大年龄 | 关 / 180 | 无绑定的 Codex 原生会话按 UUIDv7 时间戳算年龄，超龄或未来时间拒绝 400 `codex_session_identity_unavailable`（`retry: stop`）。非 v7 只计数 |

## 建议的验证顺序

1. 只看观测 24h：记录被降载账号（`error_message LIKE '%server_is_overloaded%'` 按账号占比）与「外来 token 最多的账号」是否重合，以及 `borrowed` 次数。
2. 打开严格模式 24h，对比降载占比。
3. 打开不借用（等待 20s），观察 `held` 与请求排队延迟，再对比降载占比。
4. 最后再考虑首次会话准入：先看 `expired / future` 计数是否有误伤，再决定是否长期开启。

## 已知代价

- 不借用会在高峰期增加等待（最多等待秒数），等待秒数设为 30 时不再借用，超时直接返回无可用账号。
- 首次会话准入依赖粘性绑定：绑定 TTL 1 小时；没有 Redis 时重启会丢失绑定，闲置超过 1 小时的旧会话会被拒。
- 计数与 turn-state 精确溯源都是进程内存，多实例部署不汇总。
```

In `docs/CONFIGURATION.md`, in the Codex settings section (near the `CODEX_TELEMETRY_ENABLED` row), add one line after the table:

```markdown
会话防护（turn-state 严格模式、不借用、首次会话准入）是管理后台设置项而非环境变量，说明见 [session-guards.md](session-guards.md)。
```

- [ ] **Step 2: Full verification**

Run, in the foreground, and wait for each:

```bash
gofmt -l . | grep -v third_party
go vet ./proxy/ ./auth/ ./database/ ./admin/
go test ./database/ ./auth/ ./admin/ ./proxy/ -count=1
cd frontend && npm test && npm run typecheck && cd ..
```

Expected: gofmt prints nothing; vet clean; all packages PASS (proxy ≈ several minutes, admin ≈ 9 minutes); frontend PASS + tsc clean. If any pre-existing test fails, confirm it also fails on `git stash` state before touching it; report it, do not "fix" unrelated tests.

- [ ] **Step 3: Impact check and commit**

Run `gitnexus_detect_changes()` and confirm the affected flows are limited to Responses HTTP/WS dispatch, session affinity wait, settings persistence and runtime status. Then:

```bash
git add docs/session-guards.md docs/CONFIGURATION.md
git commit -m "docs(session-guards): operator guide and verification order"
```

- [ ] **Step 4: Report**

Summarize: the five commits, the exact settings keys and defaults, the runtime status fields, and the verification order from `docs/session-guards.md`. Do not push.

---

## Self-Review

**Spec coverage**
- §1 observability → Task 4 (classification/counters), Task 5 (wiring + provenance on success for HTTP & WS), Task 8 (panel). ✔
- §2 strict mode → Task 4 (unknown strip gated, cross always header+body), Task 5 (executor WS projection), settings Tasks 1–3. ✔
- §3 no-borrow → Task 6 (direct + wait loop hold, counters), settings Tasks 1–3, panel Task 8. ✔
- §4 initial admission → Task 7 (pure evaluator, v7-only rejection, 30 s future tolerance, both entry points, 3600-bucket stats), settings Tasks 1–3, panel Task 8. ✔
- Verification/A-B guidance → Task 9 doc. ✔

**Placeholder scan**: every code step has full code; the only "look up and reuse" instructions are for pre-existing test helpers whose names must be copied from the repo (`newSettingsTestHandler` source, account injection API in proxy tests, the wait-loop `select`), each with an explicit fallback.

**Type consistency**: `applyCodexTurnStateEchoPolicy(affinityKey, account, headers, body) ([]byte, turnStateEchoClass, bool)` is used identically in Tasks 4/5; `projectCodexTurnStateForWebsocket(body, headers) ([]byte, http.Header)` in Tasks 4/5; `SessionBorrowStats{Borrowed, Held}` in Tasks 6/8; `sessionGuardInitialSnapshot(now) (recentHour, sinceStart SessionGuardInitialSummary)` in Tasks 7/8; `checkInitialSessionAdmission(headers, body, identity, hasBinding, received) *api.APIError` in Task 7 and its wiring test; settings field names match across `database.SystemSettings`, `proxy.RuntimeSettings`, admin JSON keys, and the frontend type.
