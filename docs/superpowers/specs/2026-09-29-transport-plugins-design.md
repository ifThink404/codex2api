# Transport plugins, BPS plugin and log-analysis Agent — design

Date: 2026-09-29. Branch: `feat/transport-plugins` (from `production-main` e2e589d9).

## Goals

- Alternative upstream channels ("transport plugins") can be switched on/off instantly on every replica, enabled per account group, and overridden per account. BPS is the first plugin; others (e.g. "Prism") follow the same contract.
- Every plugin gets its own admin pages: overview, packet capture, logs, errors, Agent analysis.
- An independent log-analysis Agent module calls an LLM through our own pool (a gateway API key) and is reusable by every plugin.
- Keep core hooks to one call per site so upstream merges stay trivial.

## Non-goals

- fj-server session windows/capacity (A), session identity/routing/relaxed failover (B), turn-state aliasing (C) — dropped permanently.
- Official BPS `run_officejs` tool relay (deferred).

## 1. Plugin framework (`proxy/plugins`)

Registry of compiled-in plugins. Interface (shape, refine during implementation):

- `ID() string`, `Describe() Meta` (name, endpoints supported, account eligibility).
- `Admissible(ctx, account, model) (bool, reason)` — account-filter veto (e.g. upload cooldown).
- `Select(ctx, account, model, reqKind, prior) bool` — does this plugin serve this attempt. Includes "sticky domain": once a conversation's `previous_response_id` / compaction items were produced by a plugin, later requests must stay on that plugin (opaque encrypted content is route-bound).
- `BindRequest(c, body, compact)` — per inbound request/WS frame; installs plugin-owned request state. Receives a small classification DTO: turn/session/thread IDs parsed from `x-codex-turn-metadata`, request kind (subagent/compaction/review), caller owner (verified NewAPI user, else API-key digest), device ID.
- `Execute(ctx, ReqEnv) (*http.Response, error)` — ReqEnv: account, body, downstream headers, cacheKey, proxy URL, API key, compact flag. Core supplies pooled client/Resin, UA audit, model quota, trace begin/finish.
- Response transformer: `TransformJSON`, `TransformSSEFrame`, `FilterHeaders` — applied before usage extraction. New seam in core (fj-server hid it inside the turn-state stream wrapper).
- Errors: `ClassifyPrepError`, `OnPrepFailure` (zero-token usage attempt + cooldown), terminal writer.
- Account config schema: namespaced credential keys with validate/apply, runtime Account extension, scheduler-outbox copy, import defaults/export.
- Plugin settings block (key/value JSON), `Migrate(db)` for plugin-owned tables, admin routes, connection-test mode hook.

Core wiring: resolve once per attempt where `useBPS` is computed today (`proxy/handler.go` Responses + ResponsesCompact), then ChatCompletions, Messages (where applicable), downstream WS (force HTTP when a plugin serves), images excluded unless a plugin declares support. Upstream's Excel BPS branch (`openai_excel_bps` flag) is taken over: that flag means "BPS plugin, Excel profile".

State: table `transport_plugins(id, enabled, group_ids JSON, config JSON, updated_at)`; atomic in-memory snapshot; new scheduler_outbox entity `plugin` so every replica hot-reloads. Precedence: account override > group membership (`account.InAnyGroup`) > plugin global switch. Plugins default OFF.

`usage_logs.transport` column (`native` or plugin ID) + plugin metadata JSON column (replaces fj's `bps_agent_iteration` column); UpstreamEndpoint shows the real plugin endpoint.

## 2. Capture store and pages

Table `plugin_captures(id, plugin, request_id, account_id, attempt, direction, status, headers, body, error_kind, truncated, created_at)`. Written asynchronously around plugin Execute + response transformer. Masked (`security.MaskSensitiveData`), body cap 64KB. Default OFF; per-plugin switch + sampling rate; retention 3 days via batched purge (model: `database/prompt_retention.go`) and a `StartPluginCaptureRetention` job. Joins usage logs by `request_id`.

Frontend: nav entry `/plugins`, route `/plugins/:id/:view` with views overview (switch, groups, per-account overrides, config), captures, logs (usage_logs filtered by transport), errors (reuse OperationsErrors components), agent. Shared `components/ui/` only; zh/en/zh-TW; source-guard tests.

## 3. Log-analysis Agent (`internal/logagent`)

Depends only on `type LLM interface { Respond(ctx, model, instructions, input string) (string, error) }`. Implementation uses `proxy.Handler.ExecuteInternalResponseForAPIKey` with an admin-selected gateway key and model; `internal_reason = "log_agent:<plugin>"`. Own config (key ID, model, limits). Admin endpoints accept capture IDs / usage-log request IDs / error filters from any plugin page, build a bounded, masked context, return structured findings. On-demand only.

## 4. BPS plugin

Base: fj-server's entire BPS (profiles word/excel/sheets/powerpoint, attachment upload + cache + scheduler + history trim, full/round/turn_round convergence, Word identity, task affinity & inferred session, upload 429 cooldown, fixed runtime-overhead billing, test mode auto/codex/bps).

Strip A/B/C: stub failover epoch (key "", generation 0), drop session preserve-input validation, outbound/URL privacy finalizers, synthetic turn-state; replace fingerprint with raw downstream headers + cacheKey; port small helpers (`historyItemMetadata`, `validSessionGraphUUID`, `codexIdentityDigest` byte-identical); move `ResolveCodexIdentityUUIDv7` + table `codex_identity_uuid7_values` into BPS-owned DB code (same table name). Convergence mode is its own account key `codex_bps_convergence`, not the shared fingerprint enum.

Keep from production-main's BPS (which is deleted): default OFF (incl. import defaults), model management never depends on BPS (keep independence tests), central BPS account list, quick-config toggle, badge, shared eligibility check, instant toggle. New: enable by account group. Do NOT carry: `X-Codex2API-BPS-Requested-Model` header leak, dead diagnostics, honoring client `stream`. Keep untouched: per-account model availability (`account_model_observations`), GPT-6 pricing. Credential key `codex_bps_enabled` is reused (existing rows carry over).

Adopt from official BPS (approved 2026-09-29): 1 SSE keepalive (`response.in_progress` while upstream silent); 2 cutoff completion with marker; 3 image-refusal fallback ladder + latest-turn-only failure rule (merge ideas with fj's re-upload); 4 provider error scrubbing; 5 `text.format` → prompt instruction; 6 effort normalization + agent_iteration fallback; 8 BPS failures excluded from native account health/cooldown, as a policy switch; 9 replay persisted only with identified conversation, cache-failure backoff. Deferred: 7 run_officejs relay.

Attachment 429 local fallback (pdftotext/libreoffice): ported, OFF by default, only active when binaries exist.

## Phases

1. Framework, `transport` column, plugin state + hot reload, capture store.
2. BPS plugin port; delete production-main BPS.
3. Plugin pages.
4. Log-analysis Agent.
5. Official BPS adoptions.
