# Codex 会话防护（Session Guards）设计

日期：2026-09-17  
分支：`codex/production-main`（已含 upstream/main `600636c8`）  
背景：DS 诊断报告《codex2api-账号244-健康度与断流诊断-20260910》+ 上游 PR #572（ekti123456 `codex/session-window-controls`）评估。

## 要解决的问题

共享网关下 Codex OAuth 账号被上游按「身份 × 模型」分桶降载（`server_is_overloaded`）。
报告排除了指纹/UA/出口/请求形状等静态差异，但没有检查一个动态变量：**账号历史上收到过别的账号铸造的上下文**。
现状代码里有三条把跨账号状态送上游的通路：

1. `X-Codex-Turn-State` 守卫（`proxy/codex_turn_state.go`）只剥离「已知由其他账号铸造」的回带；溯源表是进程内存、1h TTL。来源未知（重启、过期、另一实例、WS 路径从不记录）→ **原样透传**。
2. 下游 WebSocket 路径（`proxy/responses_ws.go`）**完全没有**守卫；WS v2 的 token 在请求体 `client_metadata.x-codex-turn-state` 里，从未被剥离。
3. 「会话粘性容量溢出」（`auth/store.go` `nextForSessionWithFilter`）：绑定账号并发满时，非续链请求**无条件借用**其他账号，整段会话上下文（含加密推理块、turn-state）发到借用账号。报告显示 65% 的溢出由 244 发起，借用目标恰是健康度最差的账号。

PR #572 作者的整体模型（一窗口一号、永久归属、不借用、首次绑定清 turn-state）把这个变量归零后"变稳定"。
本设计不搬 PR 代码，而是在当前 upstream API 上以**默认关闭的开关**实现四项可独立 A/B 的能力。

## 四项能力

### 1. turn-state 回带可观测（无开关，始终采集）

每个 Codex 请求在选号后、出站前，对入站 token（HTTP 头 `X-Codex-Turn-State` 或体 `client_metadata.x-codex-turn-state`）分类：

| 类别 | 判定 |
|---|---|
| `none` | 无 token |
| `same` | 溯源账号 == 本次账号 |
| `cross` | 溯源账号 != 本次账号 |
| `unknown` | 无溯源记录（绑定过期/重启/未记录） |

溯源顺序：`codexTurnStateOrigins`（精确，1h）→ `store.SessionAffinityAccountID(affinityKey)`（含 Redis 缓存）→ unknown。
为让 WS 路径与「HTTP 下游 + WS 上游」也有精确溯源，**每次成功尝试结束时都调用 `noteCodexTurnStateProvenance(affinityKey, account)`**（HTTP 与 WS 两条路径）。

计数：进程内存，按账号 `{same, cross, unknown, stripped}` + 全局；`/api/admin/runtime` 新增 `session_guards` 段；`[TURN-STATE]` 日志行只在 `cross`/`unknown` 时打印（含账号、类别、亲和键哈希、是否剥离）。

### 2. turn-state 严格模式（`codex_turn_state_strict`，默认 off）

- 无论开关：`cross` → 剥离**头 + 体**（修正现状只剥头、且 WS 路径不剥的缺口；这是上游注释声明的意图）。
- 开关 on：`unknown` 也剥离（头 + 体）。首次绑定（无绑定）时 token 必然是 unknown → 与 PR 的「首次绑定清除」等价。
- 开关 on：上游 WS 握手不再携带 `X-Codex-Turn-State`；若出站头里仍有 token（same 类）且帧体没有 `client_metadata.x-codex-turn-state`，则写入帧体后删除头。对应官方客户端契约：WS v2 的 token 走 `response.create.client_metadata`，握手头是逐连接冻结的（`proxy/handler.go:257` 注释、PR 文档 `CODEX_METADATA_FORWARDING.md:38`）。

### 3. 不借用（`codex_session_no_borrow_enabled` 默认 off，`codex_session_no_borrow_hold_seconds` 默认 20，范围 1–30）

绑定账号并发满时：
- 直接选号（等待前）拒绝借用 → 返回 nil → handler 进入现有等待循环（`WaitForDispatchAvailable`，30s）。
- 等待循环内：`elapsed < hold` 期间继续拒绝借用；`elapsed >= hold` 后允许借用（原逻辑），避免硬失败。
- 续链请求（`preserveBinding=true`）不受影响（本来就不借用）。
- 计数：`borrowed`（实际借用）、`held`（被扣住的次数），进入 `session_guards.borrow`。

hold = 30 时借用窗口为 0，等待超时后由现有逻辑返回「无可用账号」；UI 说明里注明建议 ≤ 25。

### 4. 首次会话准入（`codex_initial_session_admission_enabled` 默认 off，`codex_initial_session_max_age_seconds` 默认 180，范围 1–86400）

触发条件（全部满足）：开关 on；`affinityKey` 无现有绑定（`SessionAffinityAccountID` 未命中）；`sessionIdentity.explicitUpstreamID != ""`；Codex 原生客户端（`EvaluateEngineFingerprint(headers, body, nil)` 或 `IsCodexOfficialClientByHeaders(UA, Originator)`）。

判定：解析 `explicitUpstreamID` 为 UUID。
- 非 UUID 或非 v7 → `invalid`：**只计数，放行**（与 PR 不同：威胁模型是重放真实旧 ID，旧真实 ID 必然是 v7；伪造 v4 只会建新绑定，无可抢）。
- age = 收到时刻 − v7 时间戳；age < −30s → `future` 拒绝；age > max → `expired` 拒绝；否则 `allowed`。
- 拒绝：HTTP 400 `codex_session_identity_unavailable`，`details.retry = "stop"`，文案「当前会话无法继续处理，请重新打开对话；仍失败时请新建对话。」WS 路径写错误帧后以 `ClosePolicyViolation` 关闭（与同文件其他入口校验一致）。
- 统计：`samples/allowed/expired/future/invalid/max_age_ms/avg_age_ms`，进程启动以来 + 最近 1 小时（3600 秒桶）。

已知代价（在 UI 说明里写明）：上游粘性 TTL 1h，闲置超过 1h 的会话若未配 Redis 且重启过会失去绑定；开启本项后这类会话会被拒。**只在 Redis 缓存可用、且接受该代价时开启。**

## 不做的事

- 不搬 PR 的会话身份解析、请求诊断、窗口扩容、永久归属。
- 不新增 usage_logs 列；观测靠日志 + 运行状态计数。
- 不改 `affinity_mode` 语义；不借用只在「并发满」这一种情形生效。

## 设置项与持久化

| 键 | 类型 | 默认 | 位置 |
|---|---|---|---|
| `codex_turn_state_strict` | bool | false | RuntimeSettings |
| `codex_session_no_borrow_enabled` | bool | false | Store（atomic） |
| `codex_session_no_borrow_hold_seconds` | int 1–30 | 20 | Store（atomic） |
| `codex_initial_session_admission_enabled` | bool | false | RuntimeSettings |
| `codex_initial_session_max_age_seconds` | int 1–86400 | 180 | RuntimeSettings |

全部落 `system_settings`（PostgreSQL `ADD COLUMN IF NOT EXISTS` + SQLite `ensureSQLiteColumn`），热更新生效。
UI 放在「设置 → 调度 → 会话粘性」卡片，`session_slot_buffer` 之后；文案三语（zh / en / zh-TW）。

## 验证方式（A/B）

1. 上线后先只看观测：运行状态页 `session_guards.turn_state` 按账号的 `cross/unknown` 次数与 `borrow.borrowed`；下一波降载时对照被降载账号是否是收到外来 token / 被借用最多的账号。
2. 打开严格模式 24h，对比 `server_is_overloaded` 占比（报告 §8 的 SQL）。
3. 打开不借用（hold 20），观察 `held` 与排队延迟，再对比降载占比。
4. 首次会话准入最后开，观察 `expired/future` 拒绝数是否有误伤。
