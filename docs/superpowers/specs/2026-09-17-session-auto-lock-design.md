# 连续 500 自动锁定会话 + 首次会话准入 relay 豁免 设计

日期：2026-09-17（第二轮，基于已上线的 v2.9.8-fr-20260917.1）  
前置：`docs/superpowers/specs/2026-09-17-codex-session-guards-design.md`（会话防护第一轮）

## 1. 首次会话准入：relay 账号豁免（补缺口）

现状：`checkInitialSessionAdmission` 在选号之前判定，只看客户端是否原生 Codex，因此开启后会连最终落到中转（relay-style）账号的会话一起拒绝。

改为：判定移到**选号之后、官方 Codex 路径**里（HTTP `Responses` 的 relay 分支 `if account.IsRelayStyle()` 之后；WS `forwardResponsesWebSocketTurn` 在出站前），且**每个请求只判定一次**（首次官方账号尝试时评估，结果缓存在 gin context；后续 failover 尝试不重判）。拒绝时先 `h.store.Release(account)` 再回 400（HTTP `api.SendErrorWithStatus(..., 400)` / WS 错误帧 + `ClosePolicyViolation`）。所选账号 `IsRelayStyle()` 时直接放行不评估。其余语义（v7 年龄、30 秒未来容忍、非 v7 只计数、统计桶）不变。

## 2. 连续 500 自动锁定会话（默认关闭）

### 身份
会话键 = 现有粘性键 `affinityKey = sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)`（会话 ID + API Key；NewAPI 前置时 `X-Codex2API-Affinity-Key` 也落到同一个键）。入口（HTTP `Responses`、WS `forwardResponsesWebSocketTurn`）把它写入 gin context，供用量落库点读取。

### 计数（在 `logUsageForRequest` 统一落点）
满足全部条件才参与：开关开启；context 有会话键；`!input.IsRetryAttempt`（内部重试不重复计）；`input.InternalReason == ""`（标题/记忆等内部请求不计）；`input.AccountID` 对应账号存在且 `!IsRelayStyle()`（**API 中转请求不参与**）。
- `input.StatusCode == 500` → 该会话连击 +1；达到阈值 → 写锁（DB + 内存），连击清零，`locked_total++`。
- 其他最终状态 → 连击清零。
- 连击表进程内存、上限 50000 条，超限时清理最久未更新的条目；保存设置（开关或阈值变化）重置全部连击；重启清零。

### 锁
- 表 `session_auto_locks`（PostgreSQL/SQLite 各自 DDL，`New()` 启动时 ensure）：`id`、`session_key`（UNIQUE）、`session_id_prefix`（会话 ID 前 12 位，展示用）、`api_key_id`、`account_id`、`error_message`（触发那次的错误摘要 ≤255）、`threshold`、`source`（`automatic`）、`locked_at`、`created_at`。
- 内存 `locked map[string]struct{}`：进程首次检查时从 DB 加载一次（`LoadSessionAutoLocks`），之后本进程锁/解锁同步维护；多实例部署下另一实例的锁只在其重启后可见（文档写明）。
- 入口在会话键算出后检查：命中 → 400 `session_blacklisted`，`details.retry = "stop"`，文案「该会话因连续上游错误已被锁定，请新建对话；如需恢复请管理员解锁。」；WS 写错误帧后 `ClosePolicyViolation` 关闭。**锁检查不区分账号类型**（锁是会话级的，计数阶段已排除中转请求，因此中转会话不会被锁）。
- 关闭开关不解锁已有条目；解锁只有管理员手动。

### 设置
`codex_session_auto_lock_enabled`（bool，默认 false）、`codex_session_auto_lock_threshold`（int，默认 3，范围 1–10000，越界归 3）。RuntimeSettings + `system_settings` 两列 + admin `GET/PUT /api/admin/settings`。保存任一项变化时重置连击。

### 管理接口
- `GET /api/admin/session-locks?limit=200` → `{"locks":[{id, session_id_prefix, api_key_id, account_id, account_name, error_message, threshold, source, locked_at}], "total": N}`（`account_name` 从 store 解析，账号不存在则空）。
- `DELETE /api/admin/session-locks/:id` → 删除 DB 行并从内存移除，`unlocked_total++`；不存在返回 404。

### 运行状态
`session_guards.auto_lock = {enabled, threshold, active_locks, locked_total, unlocked_total, streak_entries}`。

### UI
- 设置 → 调度 → 会话粘性卡片，紧跟首次会话准入两项之后：开关「连续 500 自动锁定会话」+ 数字「连续错误次数」（1–10000，默认 3；开关关时置灰），沿用卡片的自动保存（无独立保存按钮）。
- 运维 → 运行状态：会话防护面板加一行「自动锁定」计数；面板下方新增卡片「已锁定会话」：表格列 = 会话前缀、API Key、账号、错误、锁定时间、操作（解锁按钮，`components/ui/button`）；空态文案；解锁后刷新。
- 三语文案（zh / en / zh-TW）。

## 不做的事
不做 fork/父子会话继承锁定；不做 NewAPI 侧签名字段；不做过载统计页；不持久化连击。

## 3. turn-state 托管（真实 token 不透传给客户端）

### 威胁
用户 A 通过网关用账号 B 拿到上游铸造的 `X-Codex-Turn-State`（T_B），带去别处（含风控账号）失败后再回来，同一枚 T_B 打回 B。现有策略只剥"异账号"token，同账号 T_B 会原样放行——B 被传染。

### 官方客户端契约（openai/codex `codex-rs`，2026-09 源码核实）
- token 每轮一枚：`ModelClientSession` 按轮创建，`OnceLock` 在本轮首个响应设置一次，本轮内所有请求原样回带，跨轮不得回带。
- 客户端接收位置：HTTP 响应头 `x-codex-turn-state`；`response.metadata` 事件的 `headers` 对象里的 `x-codex-turn-state`（SSE 与 WS 通用，`sse/responses.rs turn_state()`）；WS 升级响应头（仅客户端自己的连接）。
- 客户端回带位置：HTTP 请求头；WS `response.create.client_metadata["x-codex-turn-state"]`。
- 网关 WS 传输路径上游事件名为 `codex.response.metadata`，HTTP 路径为 `response.metadata`。

### 设计（`codex_turn_state_vault_enabled`，默认 **开**）
- **下发替身**：每当上游铸造 T（HTTP 响应头，或 `response.metadata` / `codex.response.metadata` 事件 `headers.x-codex-turn-state`），网关生成替身 S = `c2a-ts-v1.` + 32 位十六进制随机串，存入托管表 `vault[affinityKey] = {real: T, substitute: S, accountID, expiresAt: now+1h}`，把 S 写给客户端（响应头改为 S；事件 JSON 里的字段值改为 S）。事件改写点：HTTP `handler.go` 的 `sanitizeCapacityShedEventForClient(eventType, data)` 之后；WS `responses_ws.go` 同名调用之后。响应头改写点：`relayCodexTurnStateResponseHeader` 与 `commitResponsesStreamAttempt`。
- **回带还原**：`applyCodexTurnStateEchoPolicy` 在托管开启时：入站值 == `vault[affinityKey].substitute` 且托管账号 == 本次账号 → 头/体替换为真实 T（class `same`）；账号不同 → 剥离（`cross`）；入站值不是本会话当前替身（外来真实 token、过期替身、别处的替身）→ 一律剥离（`unknown`，不再受 strict 开关影响）。
- 替身是随机值，不含真实 token 信息；托管表进程内存、1 小时 TTL、按亲和键覆盖（新一轮铸造覆盖旧轮）；重启清空（客户端下一轮拿到新替身）。多实例部署没有共享托管：另一实例收到的替身无法还原会被剥离，仅损失本轮粘性路由（文档写明）。
- 关闭托管时行为回到第一轮语义（真实 token 透传、strict 决定 unknown 是否剥离）。
- 计数：`session_guards.turn_state.vault = {issued, restored, foreign_stripped}`。
