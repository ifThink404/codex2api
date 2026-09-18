# Account Policies (prompt exemption / direct egress / session guards off) + Proxy Timezone Sync + Proxy Match Display — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let each account opt out of prompt detection, the egress proxy pool and the session guards, warn when a bound proxy's timezone disagrees with the account's, and make the account UI show which pool proxy a bound URL actually is.

**Architecture:** Three `inherit`-defaulting policy columns on `accounts` flow through the existing scheduler-override plumbing (row → projection → `auth.Account` → admin PATCH → UI). Prompt detection keeps evaluating before account selection but stores a *pending block* in the gin context; a post-selection enforcement step (next to the initial-session admission check) executes or discards it depending on the selected account's policy. Egress `direct` short-circuits `resolveProxyForAccountSnapshot`. The proxy probe additionally captures the ip-api `timezone` field into `proxies.test_timezone`; the frontend compares it with the account timezone and offers a one-click sync through the existing scheduler PATCH. Proxy-pool matching moves from byte-equality to a normalized URL key on both frontend and backend.

**Tech Stack:** Go 1.2x (gin, gjson/sjson, stdlib `testing`), PostgreSQL + SQLite, React 19 + TypeScript + i18next, `components/ui/*` (Select, Switch, Button, Badge, Tooltip).

**Spec:** `docs/superpowers/specs/2026-09-18-account-policies-design.md`.

## Global Constraints

- Project CLAUDE.md: `gitnexus_impact({target, direction:"upstream"})` before editing any existing function; `gitnexus_detect_changes()` before every commit; warn on HIGH/CRITICAL (the usage-log and selection paths are CRITICAL — changes there must be additive).
- Frontend: read `DESIGN.md` before touching `.tsx`; shared `components/ui/` only (`Select`, `Switch`, `Button`, `Badge`, `Tooltip`); every new string in `zh.json`, `en.json`, `zh-TW.json`; new settings blocks asserted in a source guard test; `cd frontend && npm test && npm run typecheck`.
- Policy values verbatim: `prompt_filter_policy ∈ {inherit, exempt}`, `egress_policy ∈ {inherit, direct}`, `session_guards_policy ∈ {inherit, off}`; default and unknown/empty → `inherit`. Columns `VARCHAR(16) NOT NULL DEFAULT 'inherit'` (PG) / `TEXT DEFAULT 'inherit'` (SQLite).
- Deferred prompt block: evaluation stays before selection; enforcement runs once per request at the post-selection site (memoized in gin context); the blocked response bytes are identical to today's; exempted requests are audited with source `account_exempt`.
- Direct egress: only when `egress_policy=direct` AND `proxy_url` empty; a pinned `proxy_url` keeps priority; Resin is bypassed for direct accounts.
- Session guards off: skips turn-state classification/strip/restore, vault substitution, auto-lock counting, initial-session admission, no-borrow hold and window-number recording for that account; the substitute backstop (`c2a-ts-v1.` prefix never forwarded upstream) still applies.
- Proxy timezone: `proxies.test_timezone` VARCHAR(64)/TEXT DEFAULT ''; source = ip-api `timezone` field (IANA); never auto-applied to an account.
- Proxy URL match key (frontend + backend, identical): lowercase scheme and host, keep port and username, drop password, trim whitespace and one trailing `/`.
- Go tests use stdlib `testing` only. gofmt applies to edited files only (known-dirty upstream files: `auth/expiry_urgency_test.go auth/proxy_pool.go auth/proxy_pool_integration.go auth/refresh_scheduler.go proxy/continuity_test.go proxy/payload_rules.go proxy/resin.go proxy/wsrelay/message.go proxy/wsrelay/message_test.go admin/codex_fingerprint_mode_test.go`).
- Commits `feat(account-policy): …` / `feat(proxy): …` / `fix(…)` / `docs(…)`; one commit per task; never `git add -A` (untracked `dist/`, `oauth-subscription-status-renewal-requirements.md` exist).
- Verified anchors are quoted per task at HEAD of `codex/production-main` when the branch is created; always confirm by grepping the quoted text before editing.

---

### Task 1: Account policy columns, model fields and admin API

**Files:**
- Create: `auth/account_policies.go`, `auth/account_policies_test.go`
- Modify: `database/postgres.go` (AccountRow struct ~:40-56; `ALTER TABLE accounts` block ~:1124-1140; the three `SELECT id, name, platform, type, credentials, proxy_url, …` scans ~:6835/6869, ~:7067/7093, ~:8035/8070; update builders ~:7256 and ~:7455; `BatchAccountMetadataUpdate` ~:86-115), `database/sqlite.go` (accounts CREATE TABLE ~:118-130; column list ~:797), `database/account_list_projection.go` (~:240 SELECT, ~:257 scan), `database/scheduler_outbox.go` (PG trigger ~:191-196, SQLite trigger ~:336-341), `auth/store.go` (Account struct ~:385-400; `account.SkipWarmTier = row.SkipWarmTier` ~:5621; `ApplyAccountSchedulerOverridePatch` ~:9157), `auth/scheduler_outbox_consumer.go` (~:551), `admin/handler.go` (account payload struct ~:1654 `SkipWarmTier bool json:"skip_warm_tier"`; `updateAccountSchedulerReq` ~:2131; `accountSchedulerUpdate` ~:2155; `parseAccountSchedulerUpdate` ~:2177+; `UpdateAccountScheduler` ~:2510+; payload builders at ~:2352 and ~:6776)
- Test: `database/account_policies_test.go` (SQLite round trip + extend the PostgreSQL test file convention used by `database/session_auto_locks_postgres_test.go`), `admin/account_policies_test.go`

**Interfaces:**
- Produces: `auth.PolicyInherit = "inherit"`, `auth.PromptFilterPolicyExempt = "exempt"`, `auth.EgressPolicyDirect = "direct"`, `auth.SessionGuardsPolicyOff = "off"`; `func NormalizeAccountPolicy(field, value string) string`; `func ValidateAccountPolicy(field string) func(string) error`; `Account.PromptFilterPolicy/EgressPolicy/SessionGuardsPolicy string`; `func (a *Account) PromptFilterExempt() bool`, `func (a *Account) EgressDirect() bool`, `func (a *Account) SessionGuardsOff() bool`; `func (s *Store) ApplyAccountPolicyPatch(dbID int64, promptFilter, egress, sessionGuards *string) bool`; `database.AccountRow.{PromptFilterPolicy,EgressPolicy,SessionGuardsPolicy}`; JSON fields `prompt_filter_policy`, `egress_policy`, `session_guards_policy` on `GET /api/admin/accounts` rows and accepted by `PATCH /api/admin/accounts/:id/scheduler`.

- [ ] **Step 1: Write the failing normalization test** (`auth/account_policies_test.go`)

```go
package auth

import "testing"

func TestNormalizeAccountPolicy(t *testing.T) {
	cases := []struct{ field, in, want string }{
		{"prompt_filter_policy", "", PolicyInherit},
		{"prompt_filter_policy", "EXEMPT ", PromptFilterPolicyExempt},
		{"prompt_filter_policy", "off", PolicyInherit}, // wrong enum for this field → inherit
		{"egress_policy", "direct", EgressPolicyDirect},
		{"egress_policy", "exempt", PolicyInherit},
		{"session_guards_policy", "Off", SessionGuardsPolicyOff},
		{"session_guards_policy", "direct", PolicyInherit},
		{"unknown_field", "direct", PolicyInherit},
	}
	for _, tc := range cases {
		if got := NormalizeAccountPolicy(tc.field, tc.in); got != tc.want {
			t.Fatalf("%s %q: got %q want %q", tc.field, tc.in, got, tc.want)
		}
	}
	if err := ValidateAccountPolicy("egress_policy")("pool"); err == nil {
		t.Fatal("expected validation error for egress_policy=pool")
	}
	if err := ValidateAccountPolicy("egress_policy")("direct"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	acc := &Account{EgressPolicy: EgressPolicyDirect, PromptFilterPolicy: PromptFilterPolicyExempt, SessionGuardsPolicy: SessionGuardsPolicyOff}
	if !acc.EgressDirect() || !acc.PromptFilterExempt() || !acc.SessionGuardsOff() {
		t.Fatal("predicates must reflect the policy fields")
	}
	if (&Account{}).EgressDirect() || (&Account{}).PromptFilterExempt() || (&Account{}).SessionGuardsOff() {
		t.Fatal("zero value must be inherit")
	}
}
```

- [ ] **Step 2: Run it** — `go test ./auth/ -run TestNormalizeAccountPolicy -count=1` → FAIL (undefined symbols).

- [ ] **Step 3: Create `auth/account_policies.go`**

```go
package auth

import (
	"fmt"
	"strings"
)

// 账号级策略：三列都以 inherit 为默认——上线不改变任何行为，运维按账号逐个打开。
const (
	PolicyInherit            = "inherit"
	PromptFilterPolicyExempt = "exempt" // 命中 prompt 检测的请求落到该账号时放行
	EgressPolicyDirect       = "direct" // 不进代理池/全局代理/Resin，直连上游
	SessionGuardsPolicyOff   = "off"    // 不做 turn-state 分类剥离/托管、不计 500 连击、不做准入与不借用
)

var accountPolicyValues = map[string][]string{
	"prompt_filter_policy":  {PolicyInherit, PromptFilterPolicyExempt},
	"egress_policy":         {PolicyInherit, EgressPolicyDirect},
	"session_guards_policy": {PolicyInherit, SessionGuardsPolicyOff},
}

// NormalizeAccountPolicy 把库里/请求里的值收敛成该字段允许的枚举；空、未知、不属于该字段的值一律 inherit。
func NormalizeAccountPolicy(field, value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, allowed := range accountPolicyValues[field] {
		if value == allowed {
			return allowed
		}
	}
	return PolicyInherit
}

// ValidateAccountPolicy 供 admin PATCH 解析用：非法值直接 400，而不是静默归 inherit。
func ValidateAccountPolicy(field string) func(string) error {
	return func(value string) error {
		value = strings.ToLower(strings.TrimSpace(value))
		for _, allowed := range accountPolicyValues[field] {
			if value == allowed {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of %s", field, strings.Join(accountPolicyValues[field], "/"))
	}
}

func (a *Account) PromptFilterExempt() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.PromptFilterPolicy == PromptFilterPolicyExempt
}

func (a *Account) EgressDirect() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.EgressPolicy == EgressPolicyDirect
}

func (a *Account) SessionGuardsOff() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SessionGuardsPolicy == SessionGuardsPolicyOff
}

// ApplyAccountPolicyPatch 热更新内存账号的策略字段（nil = 不改），与 ApplyAccountSchedulerOverridePatch 同一套锁纪律。
func (s *Store) ApplyAccountPolicyPatch(dbID int64, promptFilter, egress, sessionGuards *string) bool {
	if s == nil {
		return false
	}
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	if promptFilter != nil {
		acc.PromptFilterPolicy = NormalizeAccountPolicy("prompt_filter_policy", *promptFilter)
	}
	if egress != nil {
		acc.EgressPolicy = NormalizeAccountPolicy("egress_policy", *egress)
	}
	if sessionGuards != nil {
		acc.SessionGuardsPolicy = NormalizeAccountPolicy("session_guards_policy", *sessionGuards)
	}
	acc.mu.Unlock()
	return true
}
```

Add to the `Account` struct (auth/store.go, right after `SkipWarmTier bool // 跳过 warm 层级降级` ~:392):

```go
	// 账号级策略（auth/account_policies.go）；空/未知视为 inherit。
	PromptFilterPolicy  string
	EgressPolicy        string
	SessionGuardsPolicy string
```

Check whether `Account` has a copy/clone helper used by the scheduler outbox (`auth/scheduler_outbox_consumer.go` ~:551 copies `SkipWarmTier`): add the three fields there too.

- [ ] **Step 4: Run** `go test ./auth/ -run TestNormalizeAccountPolicy -count=1` → PASS.

- [ ] **Step 5: Write the failing DB round-trip test** (`database/account_policies_test.go`, SQLite; mirror the setup used by `database/usage_log_flush_test.go`)

```go
func TestAccountPolicyColumnsRoundTrip(t *testing.T) {
	db := newTestDB(t) // use the same SQLite test constructor the neighbouring tests use
	ctx := context.Background()
	id := insertTestAccount(t, db) // the neighbouring helper that inserts a minimal account row
	rows, err := db.ListAccounts(ctx) // whichever list function scans the three SELECT sites
	if err != nil { t.Fatal(err) }
	if rows[0].PromptFilterPolicy != "inherit" || rows[0].EgressPolicy != "inherit" || rows[0].SessionGuardsPolicy != "inherit" {
		t.Fatalf("defaults must be inherit, got %+v", rows[0])
	}
	if err := db.UpdateAccountScheduler(ctx, id, AccountSchedulerUpdate{ // the update type used by admin PATCH
		PromptFilterPolicy: OptionalString{Set: true, Value: "exempt"},
		EgressPolicy:       OptionalString{Set: true, Value: "direct"},
		SessionGuardsPolicy: OptionalString{Set: true, Value: "off"},
	}); err != nil { t.Fatal(err) }
	row, err := db.GetAccountByID(ctx, id)
	if err != nil { t.Fatal(err) }
	if row.PromptFilterPolicy != "exempt" || row.EgressPolicy != "direct" || row.SessionGuardsPolicy != "off" {
		t.Fatalf("policies not persisted: %+v", row)
	}
}
```

(The helper and update-type names above must be replaced by the real ones found next to `skip_warm_tier` in `database/postgres.go` ~:7256/~:7455 — grep `add("skip_warm_tier"` and use the enclosing function's signature. Keep the assertions.)

- [ ] **Step 6: Run** `go test ./database/ -run TestAccountPolicyColumnsRoundTrip -count=1` → FAIL (unknown fields).

- [ ] **Step 7: Schema + row + scans + update builders**

PG (`database/postgres.go`, inside the `ALTER TABLE accounts ADD COLUMN IF NOT EXISTS skip_warm_tier …` block ~:1134):
```sql
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS prompt_filter_policy VARCHAR(16) NOT NULL DEFAULT 'inherit';
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS egress_policy VARCHAR(16) NOT NULL DEFAULT 'inherit';
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS session_guards_policy VARCHAR(16) NOT NULL DEFAULT 'inherit';
```
Also add the three columns to the PG fresh-install `CREATE TABLE IF NOT EXISTS accounts` definition (grep `skip_warm_tier BOOLEAN` inside the CREATE).

SQLite (`database/sqlite.go`): CREATE TABLE ~:125 add `prompt_filter_policy TEXT DEFAULT 'inherit', egress_policy TEXT DEFAULT 'inherit', session_guards_policy TEXT DEFAULT 'inherit',`; column list ~:797 add
```go
		{"accounts", "prompt_filter_policy", "TEXT DEFAULT 'inherit'"},
		{"accounts", "egress_policy", "TEXT DEFAULT 'inherit'"},
		{"accounts", "session_guards_policy", "TEXT DEFAULT 'inherit'"},
```

`AccountRow` (postgres.go ~:48, after `SkipWarmTier bool`): `PromptFilterPolicy string`, `EgressPolicy string`, `SessionGuardsPolicy string`.

Every SELECT that scans `&a.SkipWarmTier` (~:6869, ~:7093, ~:8070) and the projection (`account_list_projection.go` ~:240/:257): append `COALESCE(prompt_filter_policy,'inherit'), COALESCE(egress_policy,'inherit'), COALESCE(session_guards_policy,'inherit')` to the column list right after the `skip_warm_tier` expression and `&a.PromptFilterPolicy, &a.EgressPolicy, &a.SessionGuardsPolicy` right after `&a.SkipWarmTier` — same position in both lists (count columns and scan targets; a mismatch is a runtime error on every account load).

Update builders: next to `add("skip_warm_tier", skipWarmTier.Value)` (~:7256) add
```go
		if promptFilterPolicy.Set {
			add("prompt_filter_policy", auth.NormalizeAccountPolicy("prompt_filter_policy", promptFilterPolicy.Value))
		}
```
(and the two siblings) — if `database` must not import `auth`, normalize in the admin layer instead and keep the DB builder dumb: `add("prompt_filter_policy", promptFilterPolicy.Value)`; choose the second form if `database` does not already import `auth` (check with `grep -n '"github.com/codex2api/auth"' database/*.go`). Extend the enclosing update struct with `PromptFilterPolicy, EgressPolicy, SessionGuardsPolicy OptionalString` and, if `BatchAccountMetadataUpdate` (~:86) is the type used by the scheduler PATCH, its `HasChanges()`.

Outbox triggers (`database/scheduler_outbox.go` PG ~:191-196 and SQLite ~:336-341): add the three columns to the `AFTER UPDATE OF …` list and `OR OLD.prompt_filter_policy IS DISTINCT FROM NEW.prompt_filter_policy` (SQLite form: `IS NOT`) ×3, so policy edits reach the scheduler snapshot.

`auth/store.go` ~:5621: after `account.SkipWarmTier = row.SkipWarmTier` add
```go
	account.PromptFilterPolicy = NormalizeAccountPolicy("prompt_filter_policy", row.PromptFilterPolicy)
	account.EgressPolicy = NormalizeAccountPolicy("egress_policy", row.EgressPolicy)
	account.SessionGuardsPolicy = NormalizeAccountPolicy("session_guards_policy", row.SessionGuardsPolicy)
```
`auth/scheduler_outbox_consumer.go` ~:551: `dst.PromptFilterPolicy = src.PromptFilterPolicy` ×3.

- [ ] **Step 8: Run** `go test ./database/ -run TestAccountPolicyColumnsRoundTrip -count=1` → PASS. Also run the PostgreSQL convention test against a throwaway `postgres:16` container (see `database/session_auto_locks_postgres_test.go` for the DSN env var), adding an accounts policy round trip to a new `database/account_policies_postgres_test.go`.

- [ ] **Step 9: Admin API test** (`admin/account_policies_test.go`, mirror `admin/bootstrap_defaults_test.go` setup): PATCH `/api/admin/accounts/:id/scheduler` with `{"egress_policy":"direct"}` → 200 and `GET /api/admin/accounts` row shows `egress_policy: "direct"`; PATCH `{"egress_policy":"pool"}` → 400; a PATCH without the field leaves it unchanged; `h.store.FindByID(id).EgressDirect()` is true after the first PATCH (hot update).

- [ ] **Step 10: Admin plumbing** — `admin/handler.go`: payload struct ~:1654 add `PromptFilterPolicy string json:"prompt_filter_policy"`, `EgressPolicy string json:"egress_policy"`, `SessionGuardsPolicy string json:"session_guards_policy"` and fill them at the two payload builders (~:2352, ~:6776) from the row; `updateAccountSchedulerReq` ~:2131 add three `json.RawMessage` fields; `accountSchedulerUpdate` ~:2155 add three `database.OptionalString`; in `parseAccountSchedulerUpdate` add
```go
	promptFilterPolicy, err := parseOptionalStringField(req.PromptFilterPolicy, "prompt_filter_policy", auth.ValidateAccountPolicy("prompt_filter_policy"))
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
```
(×3) and copy into the DB update; in `UpdateAccountScheduler` after the DB write call `h.store.ApplyAccountPolicyPatch(id, optionalStringPtr(update.PromptFilterPolicy), optionalStringPtr(update.EgressPolicy), optionalStringPtr(update.SessionGuardsPolicy))` where `optionalStringPtr` returns `nil` when `!Set` (add the tiny helper next to the other parse helpers ~:3114).

- [ ] **Step 11: Run** `go test ./admin/ -run AccountPolic -count=1`, then `go vet ./auth/ ./database/ ./admin/`, `gofmt -l auth database admin` (own files only). → PASS / clean.

- [ ] **Step 12: Commit** — `gitnexus_detect_changes()`, then `git add auth/account_policies.go auth/account_policies_test.go auth/store.go auth/scheduler_outbox_consumer.go database/postgres.go database/sqlite.go database/account_list_projection.go database/scheduler_outbox.go database/account_policies_test.go database/account_policies_postgres_test.go admin/handler.go admin/account_policies_test.go && git commit -m "feat(account-policy): per-account prompt/egress/session-guard policy columns and admin API"`.

---

### Task 2: Direct egress in proxy resolution

**Files:**
- Modify: `auth/store.go` `resolveProxyForAccountSnapshot` (~:4805-4860)
- Test: `auth/account_egress_policy_test.go`

**Interfaces:** consumes `Account.EgressDirect()`; no new exports.

- [ ] **Step 1: Failing test**

```go
func TestResolveProxyForAccountDirectPolicy(t *testing.T) {
	s := newTestStoreWithProxyPool(t, []string{"socks5://pool-a:1080"}) // reuse the proxy-pool store helper used by the existing ResolveProxyForAccount tests (grep "ResolveProxyForAccount" in auth/*_test.go)
	acc := s.addTestAccount(t)                                           // same helper family
	acc.EgressPolicy = EgressPolicyDirect
	if got, direct := s.resolveProxyForAccountSnapshot(acc); got != "" || !direct {
		t.Fatalf("direct policy must bypass the pool: got %q direct=%v", got, direct)
	}
	acc.ProxyURL = "socks5://pinned:1080"
	if got, _ := s.resolveProxyForAccountSnapshot(acc); got != "socks5://pinned:1080" {
		t.Fatalf("pinned proxy keeps priority over direct: got %q", got)
	}
	acc.ProxyURL = ""
	acc.EgressPolicy = PolicyInherit
	if got, _ := s.resolveProxyForAccountSnapshot(acc); got != "socks5://pool-a:1080" {
		t.Fatalf("inherit must still use the pool: got %q", got)
	}
}
```

- [ ] **Step 2: Run** → FAIL (pool proxy returned for direct).

- [ ] **Step 3: Implement** — in `resolveProxyForAccountSnapshot`, inside the `if acc != nil { acc.mu.RLock() … }` block also read `egressDirect := acc.EgressPolicy == EgressPolicyDirect`; then, before `s.mu.RLock()`:

```go
	// 账号级出口直连：不进池、不用全局代理、不经 Resin。固定代理仍然优先（accountProxy 非空时不走这里）。
	if egressDirect && accountProxy == "" {
		return "", true
	}
```
and change `resinCarriesEgress = ResinEgressEnabled() && !acc.isRelayStyleLocked()` to `… && acc.EgressPolicy != EgressPolicyDirect`. Then grep the executor for the Resin egress decision (`grep -rn "Resin" proxy/executor.go proxy/resin.go | grep -v _test`) and make the per-attempt Resin selection consult `account.EgressDirect()` the same way it consults relay style, so a direct account never has its egress carried by Resin; add one assertion for it in the same test file if the decision is a pure function (otherwise document the site in the report).

- [ ] **Step 4: Run** `go test ./auth/ -run 'ResolveProxyForAccount|DirectPolicy' -count=1` → PASS; `go vet ./auth/`.

- [ ] **Step 5: Commit** — `git add auth/store.go auth/account_egress_policy_test.go` (+ proxy/resin.go if touched) && `git commit -m "feat(account-policy): egress_policy=direct bypasses the proxy pool and Resin"`.

---

### Task 3: Session-guards policy gating

**Files:**
- Create: `proxy/session_guards_policy.go`, `proxy/session_guards_policy_test.go`
- Modify: `proxy/turn_state_vault.go` (`turnStateVaultAppliesTo` :66), `proxy/session_guards.go` (`applyCodexTurnStateEchoPolicy` :161), `proxy/session_auto_lock.go` (`observeSessionAutoLock` :228 and the window-number recording site added by the coverage fix — grep `WindowNumber =`), `proxy/initial_session_admission.go` (`enforceInitialSessionAdmission` :219), `auth/store.go` (`sessionNoBorrowAppliesTo` — grep the comment `Codex 官方绑定账号（sessionNoBorrowAppliesTo）` ~:6996)

**Interfaces:** `func sessionGuardsActiveFor(account *auth.Account) bool` = `account != nil && !account.IsRelayStyle() && !account.SessionGuardsOff()`.

- [ ] **Step 1: Failing tests** (`proxy/session_guards_policy_test.go`): (a) an official account with `SessionGuardsPolicy=off` → `turnStateVaultAppliesTo` false, `sessionGuardsActiveFor` false; (b) `applyCodexTurnStateEchoPolicy` with that account and an inbound real cross token → header kept (no strip, no classification counter change) but an inbound `c2a-ts-v1.` value is still dropped; (c) `observeSessionAutoLock` with a 500 for that account → streak stays 0; (d) `enforceInitialSessionAdmission` returns nil for that account even with an expired v7 session id; (e) `sessionNoBorrowAppliesTo` false for that account. Model each on the neighbouring tests in `proxy/session_guards_test.go`, `proxy/session_auto_lock_test.go`, `proxy/initial_session_admission_test.go`, `auth/store_no_borrow_test.go` (find by `grep -rln "NoBorrow\|noBorrow" auth/*_test.go`).

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement** — `proxy/session_guards_policy.go`:

```go
package proxy

import "github.com/codex2api/auth"

// sessionGuardsActiveFor：会话防护（turn-state 分类/托管、500 连击、准入、不借用、窗口号）
// 只作用于官方 Codex 账号，且账号没有把 session_guards_policy 设为 off。
func sessionGuardsActiveFor(account *auth.Account) bool {
	return account != nil && account.ID() > 0 && !account.IsRelayStyle() && !account.SessionGuardsOff()
}
```
Then: `turnStateVaultAppliesTo` → `return turnStateVaultEnabled() && sessionGuardsActiveFor(account)`; in `applyCodexTurnStateEchoPolicy` add, right after the existing nil/empty guards, `if account != nil && account.SessionGuardsOff() { return h.dropUnresolvedSubstituteOnly(affinityKey, account, headers, body) }` where `dropUnresolvedSubstituteOnly` reuses the existing substitute-prefix backstop (`isCodexTurnStateSubstitute` on header + body path) and returns the same tuple as the policy without touching counters other than `foreign_stripped`; `observeSessionAutoLock`: change `if account == nil || account.IsRelayStyle() { return }` to `if !sessionGuardsActiveFor(account) { return }`; `enforceInitialSessionAdmission`: `if !sessionGuardsActiveFor(account) { return nil }`; the window-number recording site: gate with `sessionGuardsActiveFor(account)`; `auth/store.go` `sessionNoBorrowAppliesTo`: add `&& !acc.SessionGuardsOff()`.

- [ ] **Step 4: Run** `go test ./proxy/ ./auth/ -run 'SessionGuardsPolicy|TurnState|AutoLock|Admission|NoBorrow' -count=1` → PASS; `go vet ./proxy/ ./auth/`.

- [ ] **Step 5: Commit** — `git add proxy/session_guards_policy.go proxy/session_guards_policy_test.go proxy/turn_state_vault.go proxy/session_guards.go proxy/session_auto_lock.go proxy/initial_session_admission.go auth/store.go` && `git commit -m "feat(account-policy): session_guards_policy=off disables the session guards per account"`.

---

### Task 4: Deferred prompt block core

**Files:**
- Create: `proxy/prompt_filter_deferred.go`, `proxy/prompt_filter_deferred_test.go`
- Modify: `proxy/prompt_filter.go` (`inspectPromptFilterOpenAIWithBlockWriter` :61-106, `inspectPromptFilterAnthropic` :143), `proxy/responses_ws.go` (`inspectPromptFilterOpenAIForWebSocket` :1695), `database/prompt_filter.go` (`PromptFilterLogInput` ~:625 add `AccountID int64`), `database/postgres.go` (`ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS account_id BIGINT NULL` next to ~:1610 + INSERT), `database/sqlite.go` (column list ~:760 `{"prompt_filter_logs", "account_id", "INTEGER NULL"}` + CREATE), `proxy/session_guard_status.go` (counters)

**Interfaces:**
- Produces: `type pendingPromptBlock struct { evaluation promptGuardEvaluation; cfg promptfilter.Config; rawBody, signedBody []byte; endpoint, model string; transport promptfilter.Transport; executed bool }`; context key `pendingPromptBlockContextKey = "prompt_filter_pending_block"`; `func (h *Handler) inspectPromptFilterOpenAIDeferred(c *gin.Context, rawBody []byte, endpoint, model string) bool` (returns true only for the immediate hard rejects: required NewAPI identity, locked conversation); `func (h *Handler) inspectPromptFilterAnthropicDeferred(c, canonicalBody, endpoint, model) bool`; `func (h *Handler) inspectPromptFilterOpenAIForWebSocketDeferred(c, conn, rawBody, endpoint, model, policyEventID) (blocked, delegated bool)`; `func (h *Handler) enforcePendingPromptBlock(c *gin.Context, account *auth.Account) bool` (HTTP: writes the block and returns true when the request must stop); `func (h *Handler) enforcePendingPromptBlockWS(c *gin.Context, conn *websocket.Conn, account *auth.Account, policyEventID string) (blocked, delegated bool)`; counters `promptPolicyExempted`, `promptPolicyBlockedAfterSelection` (atomic.Uint64) exposed as `session_guards.prompt_policy{exempted, blocked_after_selection}`.

- [ ] **Step 1: Failing unit tests** (`proxy/prompt_filter_deferred_test.go`; build the handler the way `proxy/session_guards_test.go` builds one, with a prompt-filter config whose sensitive-word list contains `FORBIDDEN_TOKEN` and mode block — see `promptfilter.DefaultConfig()` and the existing prompt filter tests for how they enable the local filter):
  1. `inspectPromptFilterOpenAIDeferred` on a body containing `FORBIDDEN_TOKEN` returns false and leaves a `pendingPromptBlock` in the context; on a clean body returns false and no pending block; the `X-Prompt-Filter-Warning` header behaviour for warn verdicts is unchanged.
  2. `enforcePendingPromptBlock(c, exemptAccount)` returns false, clears the pending block, increments `promptPolicyExempted`, and enqueues an audit row with `Source == "account_exempt"` and `AccountID == exemptAccount.ID()` (capture via the fake DB the neighbouring tests use for `EnqueuePromptFilterLog`).
  3. `enforcePendingPromptBlock(c, inheritAccount)` writes HTTP 400 with body byte-identical to the one `inspectPromptFilterOpenAI` writes today for the same input (compute the expected bytes by calling the old function on a fresh recorder), increments `promptPolicyBlockedAfterSelection`, and locks the conversation exactly as the old path did (assert via `activePromptConversationLock`).
  4. Calling `enforcePendingPromptBlock` twice on the same context executes once (second call returns the first outcome without writing again).
  5. With no pending block, `enforcePendingPromptBlock` returns false and touches nothing.

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement** `proxy/prompt_filter_deferred.go` — refactor the tail of `inspectPromptFilterOpenAIWithBlockWriter` into two halves without changing the old function's observable behaviour:

```go
package proxy

import (
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const pendingPromptBlockContextKey = "prompt_filter_pending_block"

var (
	promptPolicyExempted              atomic.Uint64
	promptPolicyBlockedAfterSelection atomic.Uint64
)

// pendingPromptBlock 是「先判后拦」的中间态：选号前已判定为拦截，但要等选到账号后再决定
// 是否真的拦——落到 prompt_filter_policy=exempt 的账号时放行并审计。
type pendingPromptBlock struct {
	evaluation promptGuardEvaluation
	cfg        promptfilter.Config
	rawBody    []byte
	signedBody []byte
	endpoint   string
	model      string
	transport  promptfilter.Transport
	writeBlock func(*gin.Context, string)
	executed   bool
	blocked    bool
}

func (h *Handler) inspectPromptFilterOpenAIDeferred(c *gin.Context, rawBody []byte, endpoint, model string) bool {
	if c != nil && c.GetBool("prompt_intelligence_internal") {
		return false
	}
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	signedBody := ingressRequestBody(c, rawBody)
	if h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, signedBody) {
		return true
	}
	if h.rejectLockedPromptConversation(c, cfg, signedBody, rawBody, endpoint, model) {
		return true
	}
	if !promptfilter.RequiresRequestText(cfg) {
		return false
	}
	evaluation := h.evaluatePromptGuardWithConfig(c, cfg, rawBody, signedBody, endpoint, model, promptfilter.TransportHTTP)
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if evaluation.Verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", promptFilterWarningMessage(evaluation))
	}
	if evaluation.Verdict.Action != promptfilter.ActionBlock {
		return false
	}
	c.Set(pendingPromptBlockContextKey, &pendingPromptBlock{
		evaluation: evaluation, cfg: cfg, rawBody: rawBody, signedBody: signedBody,
		endpoint: endpoint, model: model, transport: promptfilter.TransportHTTP,
	})
	return false
}

// enforcePendingPromptBlock 在选号后执行一次：豁免账号放行（审计 source=account_exempt），
// 其余账号写与旧路径逐字节相同的拦截响应。返回 true 表示请求必须在此结束（调用方负责 Release/Unbind）。
func (h *Handler) enforcePendingPromptBlock(c *gin.Context, account *auth.Account) bool {
	if c == nil {
		return false
	}
	raw, ok := c.Get(pendingPromptBlockContextKey)
	if !ok {
		return false
	}
	pending, ok := raw.(*pendingPromptBlock)
	if !ok || pending == nil {
		return false
	}
	if pending.executed {
		return pending.blocked
	}
	pending.executed = true
	if account != nil && account.PromptFilterExempt() {
		promptPolicyExempted.Add(1)
		h.logPromptFilterExemption(c, pending, account)
		c.Set(pendingPromptBlockContextKey, nil)
		return false
	}
	promptPolicyBlockedAfterSelection.Add(1)
	pending.blocked = true
	h.lockPromptConversationOnLocalBlock(c, pending.cfg, pending.signedBody, pending.endpoint, pending.model, pending.evaluation.Decision, pending.evaluation.Verdict)
	if h.sendNewAPIPolicyDecision(c, pending.cfg, pending.evaluation.Decision, pending.evaluation.Verdict, pending.rawBody, pending.endpoint, pending.model, pending.signedBody) {
		return true
	}
	if pending.writeBlock != nil {
		pending.writeBlock(c, strings.TrimSpace(pending.cfg.Advanced.Enforcement.LocalBlockMessage))
		return true
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		localPromptBlockMessage(pending.cfg),
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
	return true
}

func (h *Handler) logPromptFilterExemption(c *gin.Context, pending *pendingPromptBlock, account *auth.Account) {
	h.logPromptFilterVerdictWithDecisionAndAccount(c, pending.endpoint, pending.model, "account_exempt", "", pending.evaluation.Verdict, &pending.evaluation.Decision, &pending.evaluation.Envelope, account.ID())
}

// PromptPolicyCounters 供运行状态面板读取。
func PromptPolicyCounters() (exempted, blockedAfterSelection uint64) {
	return promptPolicyExempted.Load(), promptPolicyBlockedAfterSelection.Load()
}
```

`logPromptFilterVerdictWithDecisionAndAccount` = the existing `logPromptFilterVerdictWithDecision` body with an extra `accountID int64` that is written into `input.AccountID`; make the old function a thin wrapper passing 0 (so nothing else changes). `PromptFilterLogInput.AccountID` maps to the new nullable `account_id` column in both drivers' INSERT and the log-list SELECT (the prompt-filter log admin API exposes it as `account_id`; UI change is out of scope). The Anthropic deferred variant mirrors `inspectPromptFilterAnthropic` (:143) the same way (transport HTTP, endpoint `/v1/messages`). The WS deferred variant mirrors `inspectPromptFilterOpenAIForWebSocket`: the locked-conversation branch stays immediate; on block it stores the pending block with `transport: promptfilter.TransportWebSocket` and returns `(false, false)`; `enforcePendingPromptBlockWS` performs the lock + NewAPI-verified branch (`writeNewAPIPolicyDecisionHeaders` + `writeResponsesWSError(conn, newAPILocalPromptPolicyDecisionAPIError(...))` → `(true, true)`) or the plain `writeResponsesWSError(conn, api.NewAPIError("prompt_blocked", …))` → `(true, false)`, exactly as the old function does, and records the same counters/audit.

Counters: extend `SessionGuardStatusSnapshot` (proxy/session_guard_status.go) with `PromptPolicy struct{ Exempted, BlockedAfterSelection uint64 } json:"prompt_policy"` filled from `PromptPolicyCounters()`; reset in the test reset helper used by round two.

- [ ] **Step 4: Run** `go test ./proxy/ -run 'PromptFilter|PendingPromptBlock' -count=1` → PASS (old prompt-filter tests must still pass untouched); `go vet ./proxy/ ./database/`.

- [ ] **Step 5: Commit** — `git add proxy/prompt_filter_deferred.go proxy/prompt_filter_deferred_test.go proxy/prompt_filter.go proxy/responses_ws.go proxy/session_guard_status.go database/prompt_filter.go database/postgres.go database/sqlite.go` && `git commit -m "feat(account-policy): deferred prompt block that exempt accounts can waive"`.

---

### Task 5: Wire deferral into the five entries and enforce after selection

**Files:**
- Modify: `proxy/handler.go` (:3878 Responses inspect → deferred; :4928 post-selection site — insert enforcement before `enforceInitialSessionAdmission`; :5903 compact inspect + its post-selection site inside the attempt loop right after `account, stickyProxyURL, affinityGuard = h.nextAccountForSessionWithDispatchGuard(…)` ~:6003; :6749 chat inspect + its post-selection site — grep `nextAccountForSession` inside `ChatCompletions` (~:6690+)), `proxy/handler_anthropic.go` (:494 messages inspect → deferred; post-selection site — grep `nextAccountForSession` inside `Messages` ~:435+), `proxy/responses_ws.go` (:420 WS inspect → deferred; :699 post-selection site — insert `enforcePendingPromptBlockWS` before `enforceInitialSessionAdmission`)
- Test: `proxy/prompt_filter_deferred_e2e_test.go`

**Interfaces:** consumes Task 4's functions. Rejection sequence at every HTTP site:
```go
		if h.enforcePendingPromptBlock(c, account) {
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			return
		}
```
(chat/messages/compact: use whatever release call their loop uses on early exit — grep the nearest `h.store.Release(account)` in the same loop and copy its form; if the loop has no affinity key, omit the unbind). WS site:
```go
		if blocked, delegated := h.enforcePendingPromptBlockWS(c, conn, account, policyEventID); blocked {
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			// 与 :420 处的语义一致：委托给 NewAPI 的决定保持连接，其余关闭。
			return handleWSPromptBlockOutcome(delegated) // reuse the exact statements the :420 branch executes today
		}
```

- [ ] **Step 1: Failing e2e tests** (`proxy/prompt_filter_deferred_e2e_test.go`, using the relay-branch harness from commit 586093eb's `proxy/executor_test.go`/handler e2e tests: real `Handler`, stub upstream, sensitive word `FORBIDDEN_TOKEN` in a block-mode config): (a) `/v1/responses` with the forbidden token, pool = one account with `PromptFilterPolicy=exempt` → upstream receives the request, client gets 200; (b) same with an `inherit` account → 400 `prompt_blocked`, upstream never called, account released (concurrency back to 0), affinity unbound; (c) `/v1/chat/completions` and `/v1/messages` variants of (a)/(b); (d) the WS entry: exempt → frame forwarded; inherit → error frame `prompt_blocked`; (e) a clean request on an inherit account behaves as before (no pending block).

- [ ] **Step 2: Run** → FAIL (blocked before selection regardless of policy).

- [ ] **Step 3: Wire** — replace the five `inspectPromptFilter*` calls with their deferred counterparts (keep the `return` on true: those are the immediate hard rejects) and insert the enforcement blocks quoted above at the five post-selection sites. Confirm each site by grepping `enforceInitialSessionAdmission(` (Responses HTTP + WS) and `nextAccountForSessionWithDispatchGuard(` (compact/chat/messages loops) before editing; the enforcement must run before any upstream connection is opened and before the WS handshake to the upstream.

- [ ] **Step 4: Run** `go test ./proxy/... -count=1` (full package — the prompt filter, session guard and relay e2e suites all exercise these entries) → PASS; `go vet ./proxy/...`; `go build ./...`.

- [ ] **Step 5: Commit** — `git add proxy/handler.go proxy/handler_anthropic.go proxy/responses_ws.go proxy/prompt_filter_deferred_e2e_test.go` && `git commit -m "feat(account-policy): enforce the prompt block after account selection on all Codex entries"`.

---

### Task 6: Proxy probe timezone

**Files:**
- Modify: `admin/proxy_testing.go` (`proxyProbeIPAPIFields` :29 → append `,timezone`; `proxyProbeResult` :39 add `Timezone string json:"timezone,omitempty"`; `parseIPAPIProbeBody` :264 read `result.Get("timezone")`; the ipwhois fallback parser :351 reads `timezone.id` if that provider is used), `admin/handler.go` (`persistProxyTestResult` :13274 + `TestProxy` :13307 pass `result.Timezone`), `database/postgres.go` (`UpdateProxyTestResult` :3946 signature + SQL `test_timezone = $x`; proxies `ALTER TABLE … ADD COLUMN IF NOT EXISTS test_timezone VARCHAR(64) DEFAULT ''` next to :1648; `ProxyRow.TestTimezone string json:"test_timezone"` :3611; both list SELECTs :3679/:3719 + scans), `database/sqlite.go` (proxies CREATE :420 + column list :803)
- Test: `admin/proxy_testing_timezone_test.go`, `database/proxy_timezone_test.go`

**Interfaces:** `UpdateProxyTestResult(ctx, id, expectedURL, status, ip, location, timezone string, latencyMs int) error`; proxies API rows carry `test_timezone`.

- [ ] **Step 1: Failing tests** — parser: an ip-api body `{"status":"success","query":"1.2.3.4","country":"United States","regionName":"California","city":"Los Angeles","isp":"X","timezone":"America/Los_Angeles"}` → `Timezone == "America/Los_Angeles"`; a body without `timezone` → `""`; a garbage value like `"not/a tz"` longer than 64 runes or containing spaces → `""` (validate with `time.LoadLocation` — accept only names it can load, else empty). DB: `UpdateProxyTestResult(...,"America/Los_Angeles",...)` then list → `TestTimezone` round-trips (SQLite; add the PG variant to the PostgreSQL test file).

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement** the changes listed under Files; a failed probe (`status != success`) clears `test_timezone` like it clears `test_location`.

- [ ] **Step 4: Run** `go test ./admin/ -run ProxyTest -count=1 && go test ./database/ -run Proxy -count=1` → PASS; `go vet ./admin/ ./database/`.

- [ ] **Step 5: Commit** — `git add admin/proxy_testing.go admin/handler.go admin/proxy_testing_timezone_test.go database/postgres.go database/sqlite.go database/proxy_timezone_test.go` && `git commit -m "feat(proxy): record the egress IP timezone when testing a proxy"`.

---

### Task 7: Backend proxy-URL match key for bound counts

**Files:**
- Create: `database/proxy_url_key.go`, `database/proxy_url_key_test.go`
- Modify: `database/postgres.go` `CountAccountsByProxyURL` (:3651-3675) and its consumer in `admin/handler.go` (~:13018 `boundCounts[strings.TrimSpace(p.URL)]`)

**Interfaces:** `func ProxyURLMatchKey(raw string) string` — lowercase scheme and host, keep port and username, drop password, trim spaces and one trailing `/`; invalid URLs → trimmed lowercase input. `CountAccountsByProxyURL` returns a map keyed by `ProxyURLMatchKey`; the admin consumer looks up `ProxyURLMatchKey(p.URL)`.

- [ ] **Step 1: Failing test**

```go
func TestProxyURLMatchKey(t *testing.T) {
	cases := map[string]string{
		"SOCKS5://User:Secret@Host.Example:1080/": "socks5://User@host.example:1080",
		"socks5://user:secret@host.example:1080":  "socks5://user@host.example:1080",
		"  http://h:8080  ":                        "http://h:8080",
		"http://h:8080/":                           "http://h:8080",
		"not a url":                                "not a url",
	}
	for in, want := range cases {
		if got := ProxyURLMatchKey(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** with `net/url` (`Parse`; rebuild `scheme://[user@]host[:port]`; `strings.ToLower` on scheme and hostname only). Change the SQL to `SELECT proxy_url, COUNT(*) … GROUP BY proxy_url` and fold rows into the map by `ProxyURLMatchKey` (summing). **Step 4: Run** `go test ./database/ -run ProxyURLMatchKey -count=1` and the admin proxies list test if one exists → PASS.

- [ ] **Step 5: Commit** — `git add database/proxy_url_key.go database/proxy_url_key_test.go database/postgres.go admin/handler.go` && `git commit -m "fix(proxy): count bound accounts by a normalized proxy URL key"`.

---

### Task 8: Frontend — policy selects, badges, normalized proxy match, timezone hint, runtime row, i18n

**Files:**
- Create: `frontend/src/lib/proxyUrlMatch.ts`, `frontend/src/lib/proxyUrlMatch.test.mjs`, `frontend/src/lib/accountPolicies.test.mjs` (source guard), `frontend/src/components/ProxyTimezoneHint.tsx`
- Modify: `frontend/src/types.ts` (`AccountRow` :269 add `prompt_filter_policy?: 'inherit' | 'exempt'`, `egress_policy?: 'inherit' | 'direct'`, `session_guards_policy?: 'inherit' | 'off'`; the proxy pool entry type that has `test_location` add `test_timezone?: string`; `RuntimeStatusResponse.session_guards` add `prompt_policy?: { exempted: number; blocked_after_selection: number }`), `frontend/src/api.ts` (the scheduler PATCH payload type gains the three optional fields), `frontend/src/pages/Accounts.tsx` (state :1878 area; load :5533; reset :5610; payload :5756; dialog JSX after the skip-warm switch block ending ~:9645; list badges next to the proxy badge — the row component that calls `resolveAccountProxyBinding(`; quick editor `<AccountProxyQuickEditor …>` ~:9979), `frontend/src/components/ProxyField.tsx` (:88 match + hint slot), `frontend/src/components/ProxyPoolSelect.tsx` (:129 match), `frontend/src/lib/accountProxyBinding.ts` (`resolveAccountProxyBinding` :137 — `ctx.managed.get(accountProxy)` becomes a lookup by match key; build `ctx.managed` keyed by `proxyUrlMatchKey`), `frontend/src/components/AccountProxyQuickEditor.tsx` (hint + sync button), `frontend/src/pages/RuntimeStatus.tsx` (guards panel row after the auto-lock row), locales `zh.json`/`en.json`/`zh-TW.json`

**Interfaces:**
- `proxyUrlMatchKey(raw: string): string` — same rules as the Go `ProxyURLMatchKey` (test with the same table).
- `ProxyTimezoneHint({ proxyTimezone, accountTimezone, onSync, syncing })` renders nothing when either is empty or equal; otherwise an amber `text-xs` line with `t('accounts.proxyTimezoneMismatch', { proxy, account })` and a `Button size="sm" variant="outline"` labelled `t('accounts.proxyTimezoneSync', { tz })`.
- i18n keys (`accounts.*`): `policyPromptFilterLabel` 「prompt 检测」/ "Prompt detection" / 「prompt 檢測」; `policyPromptFilterHint`; `policyPromptFilterInherit` 「继承全局」/ "Inherit"; `policyPromptFilterExempt` 「豁免（落到此账号的命中请求放行）」/ "Exempt (matched requests routed here pass)"; `policyEgressLabel` 「出口」/ "Egress"; `policyEgressDirect` 「直连（不进代理池）」/ "Direct (bypass proxy pool)"; `policyEgressHint`; `policySessionGuardsLabel` 「会话防护」/ "Session guards"; `policySessionGuardsOff` 「关闭」/ "Off"; `policySessionGuardsHint`; badges `badgePromptExempt` 「豁免检测」, `badgeEgressDirect` 「直连」, `badgeGuardsOff` 「防护关」; `proxyTimezoneMismatch` 「出口 IP 时区 {{proxy}}，账号时区 {{account}}」; `proxyTimezoneSync` 「同步为 {{tz}}」; `proxyTimezoneSynced` 「时区已同步」; `proxyPoolMatched` 「代理池：{{label}} · {{ip}} · {{location}} · {{latency}} ms」; `proxyPoolUnmatched` 「不在代理池中」; `badgeTimezoneMismatch` 「时区不一致」; runtime `runtime.promptPolicy` 「prompt 豁免」, `runtime.promptExempted` 「已豁免」, `runtime.promptBlockedAfterSelection` 「选号后拦截」. Provide en and zh-TW for each.

- [ ] **Step 1: Failing guard + unit tests** — `proxyUrlMatch.test.mjs` with the Task 7 table; `accountPolicies.test.mjs` (pattern: `frontend/src/lib/sessionGuards.test.mjs`) asserting: `types.ts` has the three policy fields and `test_timezone`; `Accounts.tsx` contains `prompt_filter_policy`, `egress_policy`, `session_guards_policy`, imports `Select` from `@/components/ui/select`, references `ProxyTimezoneHint` and `proxyUrlMatchKey`; `ProxyField.tsx`/`ProxyPoolSelect.tsx`/`accountProxyBinding.ts` reference `proxyUrlMatchKey` and contain no `p.url === `; `RuntimeStatus.tsx` references `runtime.promptPolicy`; all listed keys exist in the three locales; no raw `<select`/`<input type="checkbox"` in the touched files.

- [ ] **Step 2: Run** `cd frontend && npm test` → the two new files FAIL.

- [ ] **Step 3: Implement** — `proxyUrlMatch.ts`:

```ts
export function proxyUrlMatchKey(raw: string): string {
  const trimmed = raw.trim().replace(/\/$/, '')
  try {
    const u = new URL(trimmed)
    const host = u.hostname.toLowerCase()
    const port = u.port ? `:${u.port}` : ''
    const user = u.username ? `${u.username}@` : ''
    return `${u.protocol.replace(/:$/, '').toLowerCase()}://${user}${host}${port}`
  } catch {
    return trimmed.toLowerCase()
  }
}
```
Scheduler dialog: three `Select` controls (`components/ui/select`) with the two options each, state `promptFilterPolicy/egressPolicy/sessionGuardsPolicy` initialised from the account (`?? 'inherit'`), included in the PATCH payload next to `skip_warm_tier`. Badges in the account row next to the proxy badge (`Badge variant="outline"`, only when the value is not `inherit`). `ProxyField`: compute `matched = proxies.find(p => proxyUrlMatchKey(p.url) === proxyUrlMatchKey(value))`; under the input render `accounts.proxyPoolMatched` (label/test_ip/test_location/test_latency_ms) or `accounts.proxyPoolUnmatched` when the value is non-empty and unmatched; accept optional props `accountTimezone`, `onSyncTimezone` and render `<ProxyTimezoneHint proxyTimezone={matched?.test_timezone} accountTimezone={accountTimezone} onSync={…} />`. `AccountProxyQuickEditor`: pass the account's `timezone` and a sync handler that calls `api.updateAccountScheduler(account.id, { timezone })` then reloads (toast `accounts.proxyTimezoneSynced`). Account list: `badgeTimezoneMismatch` when the resolved binding's proxy has `test_timezone` and it differs from `account.timezone` (OAuth rows only). `RuntimeStatus.tsx`: row `[t('runtime.promptPolicy'), `${t('runtime.promptExempted')} ${formatNumber(g.prompt_policy?.exempted ?? 0)} · ${t('runtime.promptBlockedAfterSelection')} ${formatNumber(g.prompt_policy?.blocked_after_selection ?? 0)}`]` after the auto-lock row.

- [ ] **Step 4: Run** `cd frontend && npm test && npm run typecheck` → PASS / clean.

- [ ] **Step 5: Commit** — `git add frontend/src/lib/proxyUrlMatch.ts frontend/src/lib/proxyUrlMatch.test.mjs frontend/src/lib/accountPolicies.test.mjs frontend/src/components/ProxyTimezoneHint.tsx frontend/src/components/ProxyField.tsx frontend/src/components/ProxyPoolSelect.tsx frontend/src/components/AccountProxyQuickEditor.tsx frontend/src/lib/accountProxyBinding.ts frontend/src/pages/Accounts.tsx frontend/src/pages/RuntimeStatus.tsx frontend/src/types.ts frontend/src/api.ts frontend/src/locales/zh.json frontend/src/locales/en.json frontend/src/locales/zh-TW.json` && `git commit -m "feat(account-policy): account policy selects, proxy match display and timezone sync hint"`.

---

### Task 9: Docs, full verification, commit

**Files:**
- Modify: `docs/session-guards.md` (new section「账号级策略」: the three fields, the deferred prompt block, direct egress, the timezone hint), `docs/CONFIGURATION.md` (one sentence pointing to the account-level policies next to the existing session-guards pointer)

- [ ] **Step 1: Docs** — add to `docs/session-guards.md` after the 已知代价 list:

```markdown
## 账号级策略

账号管理 → 调度设置里每个账号有三个策略，默认都「继承全局」：

| 字段 | 取值 | 作用 |
|---|---|---|
| `prompt_filter_policy` | inherit / exempt | exempt：prompt 检测仍在选号前评估，但命中的请求落到该账号时放行；审计 source=`account_exempt` 并记录账号 ID。落到其他账号照拦（响应与原来完全一致）。每请求只判一次，failover 不重判 |
| `egress_policy` | inherit / direct | direct 且未填固定代理：直连上游，不进代理池/全局代理/Resin；填了固定代理仍走固定代理 |
| `session_guards_policy` | inherit / off | off：该账号不做 turn-state 分类剥离与托管、不计 500 连击、不做首次会话准入与不借用、不记窗口号；网关铸造的替身仍不会被转发到上游 |

代理探测现在会记录出口 IP 的时区（`test_timezone`）；账号绑定的代理时区与账号时区不一致时，编辑页与列表会提示并可一键同步，不会自动改。代理池匹配改为按 scheme+host+port(+用户名) 规范化比较，凭据写法差异不再导致"不在代理池中"。
```

- [ ] **Step 2: Full verification** (foreground):

```bash
gofmt -l database auth proxy admin | grep -v -E '^(auth/expiry_urgency_test.go|auth/proxy_pool.go|auth/proxy_pool_integration.go|auth/refresh_scheduler.go|proxy/continuity_test.go|proxy/payload_rules.go|proxy/resin.go|proxy/wsrelay/message.go|proxy/wsrelay/message_test.go|admin/codex_fingerprint_mode_test.go)$'
go vet ./proxy/... ./auth/ ./database/ ./admin/
go build ./...
go test ./database/ ./auth/ ./admin/ ./proxy/... -count=1
cd frontend && npm test && npm run typecheck && cd ..
```
Expected: gofmt prints nothing; vet/build clean; all PASS; frontend PASS + tsc clean. Also run the PostgreSQL tests against a throwaway `postgres:16` container.

- [ ] **Step 3: Commit** — `gitnexus_detect_changes()`, then `git add docs/session-guards.md docs/CONFIGURATION.md && git commit -m "docs(account-policy): per-account policies, timezone hint and proxy matching"`.
