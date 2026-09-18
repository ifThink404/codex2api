# Auto-lock Counting Fix + Usage Turn-State Columns + First-Response Timing Headers — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop counting upstream capacity-shed 500s toward session auto-lock (and fix its runtime `enabled` display), record per-request turn-state facts in usage logs with filters and an account widget, and replace the early-200 preflight passthrough with loose first-response timing headers for NewAPI.

**Architecture:** (1) `UsageLogInput.CapacityShed` is set where the stream outcome / error payload is already classified (`isCapacityShedPayload`, `outcome.capacityShed`), and `observeSessionAutoLock` ignores those 500s. (2) Turn-state facts are captured at the existing echo-policy and issue sites into per-request/per-attempt context slots, folded into `UsageLogInput` in the `logUsageForRequest` population chain (same pattern as the window number), persisted in three new `usage_logs` columns, filtered in the DB query builder, and surfaced in the usage list, export and an account-latest batch query. (3) `codex_preflight_sse_passthrough_enabled` keeps its key but now only enables `upstreamFirstResponseTiming` headers staged at the normal commit (port of ekti a9518df6 onto our current `Responses` loop); `continuousRetryPreflightPassthrough` returns false.

**Tech Stack:** Go 1.2x (gin, gjson, stdlib `testing`), PostgreSQL + SQLite, React 19 + TypeScript + i18next, `components/ui/*`.

**Spec:** `docs/superpowers/specs/2026-09-18-turn-state-usage-design.md`

## Global Constraints

- Project CLAUDE.md: `gitnexus_impact({target, direction:"upstream", repo:"codex2api"})` before editing any existing function; `gitnexus_detect_changes({repo:"codex2api"})` before every commit; the usage-log write path and `Responses` loop are CRITICAL — changes must be additive.
- Frontend: read `DESIGN.md` first; shared `components/ui/` only (`Select`, `Tooltip`, `Badge`, `Button`); every new string in `zh.json`, `en.json`, `zh-TW.json`; source guard test for new blocks; `cd frontend && npm test && npm run typecheck`.
- Column definitions verbatim: `turn_state_length INT NULL` (PG `INT`, SQLite `INTEGER`), `turn_state_echo VARCHAR(16) DEFAULT ''` / `TEXT DEFAULT ''`, `turn_state_stripped BOOLEAN DEFAULT FALSE` / `INTEGER DEFAULT 0`. `turn_state_echo ∈ {'', none, same, cross, unknown, substitute}`. Filters verbatim: `turn_state=received|missing|not_recorded`, `turn_state_length=N` (non-negative int), `turn_state_echo=<class>`, `turn_state_stripped=true|false`; invalid → 400.
- Timing headers verbatim: `X-Codex2API-Response-Timing: v1-loose`, `X-Codex2API-First-Response-Ms`, `X-Codex2API-Attempt-First-Response-Ms`; only on the official `/v1/responses` path; never on relay paths or native WS clients; omitted when a heartbeat already committed headers; failed buffered attempts never publish.
- Auto-lock: `StatusCode == 500 && !CapacityShed` increments; capacity-shed 500s neither increment nor reset; default stays off.
- Go tests stdlib `testing` only; gofmt on edited files only (known-dirty upstream files: `auth/expiry_urgency_test.go auth/proxy_pool.go auth/proxy_pool_integration.go auth/refresh_scheduler.go proxy/continuity_test.go proxy/payload_rules.go proxy/resin.go proxy/wsrelay/message.go proxy/wsrelay/message_test.go admin/codex_fingerprint_mode_test.go`). Run the proxy suite alone (`-timeout 25m`).
- Commits `fix(session-guards): …` / `feat(usage): …` / `feat(responses): …` / `docs(…)`; one commit per task; never `git add -A` (untracked `dist/`, `oauth-subscription-status-renewal-requirements.md`).
- Anchors quoted below were verified at `086735ce`; always confirm by grepping before editing.

---

### Task 1: Auto-lock ignores capacity-shed 500s; runtime `enabled` display

**Files:**
- Modify: `database/postgres.go` (`UsageLogInput` struct — add `CapacityShed bool`, no column), `proxy/session_auto_lock.go` (`observeSessionAutoLock` ~:228-262; `sessionAutoLockSnapshot` ~:334), `proxy/handler.go` (the `database.UsageLogInput{` literals on the Responses/compact/chat paths where `outcome.capacityShed` or `isCapacityShedPayload(errBody)` is known — grep `capacityShed` and `isCapacityShedPayload(`), `proxy/handler_anthropic.go` (same for messages), `proxy/logusage*.go`/`handler.go:~1534` population chain (fallback classification from `ErrorMessage`)
- Test: `proxy/session_auto_lock_test.go` (extend), `proxy/session_guard_status_test.go` or the runtime-status handler test

**Interfaces:**
- Produces: `database.UsageLogInput.CapacityShed bool`; `func isCapacityShedErrorMessage(message string) bool` (prefix match on `server_is_overloaded`, `slow_down`, `service_unavailable_error` before the first ` · `).

- [ ] **Step 1: Failing tests** — in `proxy/session_auto_lock_test.go` add: (a) three `StatusCode: 500` inputs with `CapacityShed: true` on the same key → no lock, streak count unchanged (0); (b) two real 500s then one capacity-shed 500 then one real 500 → lock fires on the third real 500 (shed did not reset); (c) `isCapacityShedErrorMessage("server_is_overloaded · service_unavailable_error · Our servers…")` true, `("server_error · An error occurred…")` false, `("")` false. For the display: a test that calls `ApplyRuntimeSettingsFromSystem` with `CodexSessionAutoLockEnabled: true`, then asserts `sessionAutoLockSnapshot(h).Enabled == true` AND the JSON produced by the admin runtime-status handler's `session_guards.auto_lock.enabled` is true (drive the real handler as the existing runtime-status tests do; if an `authCacheProxy` branch exists, test both branches).
- [ ] **Step 2: Run** `go test ./proxy/ -run 'AutoLock|SessionGuardStatus' -count=1` → FAIL.
- [ ] **Step 3: Implement** — add `isCapacityShedErrorMessage` next to `isCapacityShedPayload` (proxy/handler.go ~:2456) reusing the same code list; in the population chain right before `h.observeSessionAutoLock(c, input)` set `input.CapacityShed = input.CapacityShed || (input.StatusCode == 500 && isCapacityShedErrorMessage(input.ErrorMessage))`; at the literals where `outcome.capacityShed` / `isCapacityShedPayload(errBody)` is already computed set `CapacityShed:` explicitly (list every site in the report). In `observeSessionAutoLock` replace `if input.StatusCode != 500 { delete…; return }` with: non-500 → delete + return (unchanged); 500 && CapacityShed → unlock mutex and return without touching the streak; else count. Fix the display: trace how the admin runtime-status handler obtains `session_guards` (grep `SessionGuardStatusSnapshotForHandler` and the `authCacheProxy` branch in admin/handler.go) and make every branch use the live `sessionAutoLockSnapshot(h)`; explain the root cause in the report.
- [ ] **Step 4: Run** the tests → PASS; `go vet ./proxy/ ./admin/`; `go test ./admin/ -run 'Runtime' -count=1`.
- [ ] **Step 5: Commit** — `git add …` (touched files only) && `git commit -m "fix(session-guards): auto-lock ignores capacity-shed 500s and reports its live enabled flag"`.

---

### Task 2: Usage turn-state columns, capture, filters, export, account-latest query

**Files:**
- Create: `proxy/usage_turn_state.go`, `proxy/usage_turn_state_test.go`, `database/usage_turn_state_test.go`, `database/account_turn_state_latest.go`, `database/account_turn_state_latest_test.go`
- Modify: `database/postgres.go` (usage_logs `ALTER TABLE … ADD COLUMN IF NOT EXISTS` block next to `upstream_response_model`; fresh-install CREATE; `UsageLogInput` + `UsageLog` row structs; both INSERTs (~:4851/:4963 — keep placeholder counts aligned); the SELECT/scan sites that read `upstream_response_model`; `UsageLogFilter` ~:6114 add `TurnState string`, `TurnStateLength *int`, `TurnStateEcho string`, `TurnStateStripped *bool`; `hasFilters` ~:6149; cache-key writer ~:6195; WHERE builder ~:6275 pattern), `database/sqlite.go` (CREATE + column list next to `upstream_response_model`), `admin/handler.go` (usage list filter parsing next to `filter.UltraOnly, ok = parseUsageLogBoolFilter(c, "ultra")` ~:8309; export columns in `admin/usage_log_export.go`; accounts list payload — add `latest_turn_state` per row from the batch query), `proxy/session_guards.go` (`applyCodexTurnStateEchoPolicy` ~:161 — record class + stripped into the request context), `proxy/codex_turn_state.go` (`relayCodexTurnStateResponseHeader` ~:40, `commitResponsesStreamAttempt` ~:70 — record real-token length into the attempt slot), `proxy/turn_state_vault.go` (`vaultCodexTurnStateEvent` ~:153 — same), `proxy/handler.go` population chain (~:1534 next to `populateUsageWindowNumber`)

**Interfaces:**
- Produces: context keys `usageTurnStateEchoContextKey`, `usageTurnStateLengthContextKey` (per attempt: `*usageTurnStateAttempt{checked bool; length int}` reset at the start of each attempt via `beginUsageTurnStateAttempt(c)`); `func (h *Handler) populateUsageTurnState(c *gin.Context, input *database.UsageLogInput)`; `UsageLogInput.TurnStateLength *int`, `.TurnStateEcho string`, `.TurnStateStripped bool`; `UsageLog` JSON `turn_state_length` (nullable), `turn_state_echo`, `turn_state_stripped`; `db.GetAccountLatestTurnStates(ctx, ids []int64, now time.Time) (map[int64]AccountLatestTurnState, error)` with `AccountLatestTurnState{CreatedAt time.Time; TurnStateLength *int; TurnStateEcho string; TurnStateStripped bool}` exposed on account rows as `latest_turn_state`.

- [ ] **Step 1: Failing tests** — DB: SQLite round trip of the three columns (NULL length preserved), filter builder cases for each of the four filters (received = `turn_state_length > 0`, missing = `= 0`, not_recorded = `IS NULL`), PG convention test (throwaway `postgres:16`); account-latest batch query returns one row per account (latest by created_at, id) with NULL length preserved and ignores non-end-user rows (reuse `endUserUsageLogPredicate`). Proxy: `populateUsageTurnState` on (a) official HTTP path with an issued token → length = len(token), echo `none`; (b) inbound same-account substitute restored → echo `substitute`, stripped false; (c) inbound cross token → echo `cross`, stripped true; (d) relay account → all NULL/''; (e) an attempt that failed before the upstream response → length NULL; (f) a stale event from attempt 1 arriving during attempt 2 → not counted for attempt 2 (`beginUsageTurnStateAttempt` isolation). Use the handler/stub-upstream harness from `proxy/upstream_model_coverage_test.go`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** — capture: in `applyCodexTurnStateEchoPolicy` after classification store `{class, stripped}` in the context only if not already set (first decision wins); at the three issue sites, after obtaining the real token, `markUsageTurnStateChecked(c, len(token))` (0 when checked but absent — call it also where the upstream response headers/metadata have been examined without a token, i.e. `relayCodexTurnStateResponseHeader` with empty token); `beginUsageTurnStateAttempt(c)` at the top of each attempt in the Responses HTTP loop (~:4020 selection) and the WS turn loop; `populateUsageTurnState` in the chain after `populateUsageWindowNumber`, gated to `sessionGuardsActiveFor(account)` (official OAuth only; else leave NULL/''). Persistence + filters + export + account-latest per the file list; the accounts list handler batches `GetAccountLatestTurnStates` for the page's account ids only.
- [ ] **Step 4: Run** `go test ./database/ ./proxy/... ./admin/ -count=1 -timeout 25m` → PASS; PG test green; `go vet`.
- [ ] **Step 5: Commit** — `git commit -m "feat(usage): record turn-state length, echo class and strip per request with filters and an account-latest view"`.

---

### Task 3: First-response timing headers (port of a9518df6)

**Files:**
- Create: `proxy/upstream_first_response.go` (port verbatim from `git show a9518df6:proxy/upstream_first_response.go`, adapting `isLooseFirstTokenResult` to our current classifier name — grep `isLooseFirstToken`), `proxy/upstream_first_response_test.go`
- Modify: `proxy/continuous_retry.go` (`continuousRetryPreflightPassthrough` → `return false` with the comment from a9518df6), `proxy/runtime_config.go` (comment on `CodexPreflightSSEPassthrough` ~:123), `proxy/handler.go` `Responses` loop (after `ExecuteRequest`/`resp` is obtained: `clearUpstreamFirstResponseHeaders(resp.Header)` + `upstreamTiming := upstreamFirstResponseTiming{enabled: CurrentRuntimeSettings().CodexPreflightSSEPassthrough, requestStart: handlerStart, attemptStart: start}`; in both SSE parse loops right after `stageTurnStateMetadataHeader(...)`: `upstreamTiming.observe(resp.Header, parsed, time.Now())`), `proxy/codex_turn_state.go` (`relayCodexTurnStateResponseHeader` → `relayUpstreamFirstResponseHeaders(c, headers)` first; `commitResponsesStreamAttempt` → relay when staging headers, and `clearUpstreamFirstResponseHeaders(c.Writer.Header())` on the failure branch), admin settings UI copy (frontend `settings.codexPreflightSsePassthrough*` keys in zh/en/zh-TW — rename copy to「向 NewAPI 上报宽松首响应（不再提前提交 200）」+ description; key names unchanged), `docs/newapi-first-response-timing.md` (port and adapt)

**Interfaces:** as in the ported file: `upstreamFirstResponseTiming{enabled, recorded, requestStart, attemptStart}`, `.observe(headers, event, now)`, `clearUpstreamFirstResponseHeaders(headers)`, `relayUpstreamFirstResponseHeaders(c, headers)`.

- [ ] **Step 1: Failing tests** — unit: `observe` records only on a loose-first-token event (an `output_text.delta`), ignores `ping`/`keepalive`/`heartbeat`, `response.created` and `error`; records once; attempt ms ≤ request ms; disabled → no headers. Integration (stub upstream, `CodexPreflightSSEPassthrough=true`): the client response carries the three headers with sane values; no early 200 before the first content event (assert the response header is written together with the first content chunk, not before — compare against the buffered path); a failed first attempt followed by a successful second attempt publishes only the second attempt's timing and the request-level ms includes the first attempt; relay account → no headers; when a heartbeat already committed headers → omitted.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** per file list. **Step 4: Run** `go test ./proxy/ -count=1 -timeout 25m` → PASS; `cd frontend && npm test && npm run typecheck`.
- [ ] **Step 5: Commit** — `git commit -m "feat(responses): report loose first-response timing headers instead of early metadata passthrough"`.

---

### Task 4: Frontend — usage turn-state column + filters, account-latest widget, settings copy

**Files:**
- Create: `frontend/src/components/UsageTurnState.tsx` (adapt from `git show 68f00806:frontend/src/components/UsageTurnState.tsx`, adding echo/stripped to the tooltip), `frontend/src/components/AccountLatestTurnState.tsx` (adapt from `git show cb53c327:frontend/src/components/AccountLatestTurnState.tsx`), `frontend/src/lib/usageTurnState.test.mjs` (source guard)
- Modify: `frontend/src/types.ts` (`UsageLog` + `turn_state_length: number | null`, `turn_state_echo`, `turn_state_stripped`; `UsageLogQueryParams` + `turn_state`, `turn_state_length`, `turn_state_echo`, `turn_state_stripped`; account row `latest_turn_state?`), `frontend/src/api.ts` (pass the four params), `frontend/src/pages/Usage.tsx` (column next to the model cell in both layouts; click-to-filter; three `Select`s in More Filters), the account health bar component (grep `AccountHealthBar`) to render `AccountLatestTurnState`, locales (`usage.turnState.*`: `notRecorded` 未记录 / `missing` 未获取 / `characters` `{{count}} 字符` / `hint` / `echo.none|same|cross|unknown|substitute` / `stripped` 已剥离 / `clickFilter` 点击按此状态筛选; `accounts.healthBarTurnState` 「最近 turn-state：{{value}}」; en + zh-TW)

- [ ] **Step 1: Guard test RED** (asserts components exist and import only `components/ui`, Usage.tsx references `turn_state_echo` and `UsageTurnState`, the health bar references `AccountLatestTurnState`, keys in three locales). **Step 2: Implement.** **Step 3:** `cd frontend && npm test && npm run typecheck` → PASS. **Step 4: Commit** — `git commit -m "feat(usage): turn-state column, filters and account-latest widget"`.

---

### Task 5: Docs, full verification, commit

- [ ] Docs: `docs/session-guards.md` — auto-lock row: add「只计真正的 server_error；server_is_overloaded / slow_down 等容量降载不计也不清零」; new section「用量日志 turn-state 列」(the three columns, the filters, the account widget, how to read "降智账号" from `turn_state_echo=cross` / `turn_state_stripped=true` by account); `docs/newapi-first-response-timing.md` from Task 3; `docs/CONFIGURATION.md` pointer sentence updated (preflight switch semantics).
- [ ] Verification (foreground): `gofmt` (filtered), `go vet ./proxy/... ./auth/ ./database/ ./admin/`, `go build ./...`, `go test ./database/ ./auth/ ./admin/ -count=1`, `go test ./proxy/... -count=1 -timeout 25m` (alone), `cd frontend && npm test && npm run typecheck`, PG convention tests on a throwaway `postgres:16` (known pre-existing failure `TestImageUserBillingPersistencePostgres` reported separately).
- [ ] Commit — `git commit -m "docs(session-guards): auto-lock counting, usage turn-state columns and first-response timing"`.
