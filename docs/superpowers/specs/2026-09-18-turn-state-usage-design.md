# 自动锁定口径修正 + 用量日志 turn-state 记录/筛选 + 首响应计时头 设计

日期：2026-09-18（第五轮；基于 v2.9.8-fr-20260918.5 之后的 `codex/production-main`）
前置：`docs/superpowers/specs/2026-09-17-session-auto-lock-design.md`（自动锁定）、`docs/superpowers/specs/2026-09-17-codex-session-guards-design.md`（turn-state 分类）、上游 ekti 分叉提交 68f00806 / cb53c327 / a9518df6（参考实现，不整体合并）

## 背景

线上开启连续 500 自动锁定 6 小时锁了 24 个会话，全部由坏桶官方号的 `server_is_overloaded` 触发：上游按账号×模型分桶降载，与会话无关，锁会话只把账号问题转嫁给用户。同时运行状态里 `auto_lock.enabled` 显示为 false 而锁仍在产生。用户要"确认降智账号"，需要按请求看 turn-state 是否拿到、回带是什么类别，而不是只有进程内计数。DS 报告 P0-1 指出 `codex_preflight_sse_passthrough_enabled` 提前提交 200 是断流体感的首要放大器，但关掉它会丢 NewAPI 的首响应计时；ekti 的 a9518df6 用私有响应头在正常提交时上报计时，可两全。

## 1. 自动锁定：容量降载不计连击

- `database.UsageLogInput` 新增 `CapacityShed bool`：在 Responses / compact / chat / messages 各落库点，凡 stream outcome 的 `capacityShed` 为真、或上游错误体经 `isCapacityShedPayload` 判定为 `server_is_overloaded` / `slow_down`（含 `service_unavailable_error` 这类同义错误类型）、或 `ErrorMessage` 以这些错误码开头者置真。
- `observeSessionAutoLock`：`StatusCode == 500 && !CapacityShed` 才 +1；`CapacityShed` 的 500 既不加也不清零（连击保持不变），其他非 500 终态照旧清零。理由：降载是上游桶的瞬时信号，与会话无关；只对真正的 `server_error` 计数。
- 运行状态 `auto_lock.enabled` 必须反映 `CurrentRuntimeSettings()` 的当前值：排查 `sessionAutoLockSnapshot` 到 admin runtime-status 之间的路径（含 `authCacheProxy` 分支）为何返回 false，修正并加测试（设置打开 → 快照 enabled=true；关闭 → false）。
- 默认仍关闭；文档写明"只计真正的 server_error"。

## 2. 用量日志 turn-state 列与筛选

每条用量日志记录本次**胜出尝试**的 turn-state 情况（只看官方 Codex 路径，其他账号类型留空/NULL）：

| 列 | 类型 | 含义 |
|---|---|---|
| `turn_state_length` | INT NULL | 上游首次返回的真实 `X-Codex-Turn-State` 的字符数（HTTP 头或 SSE/WS metadata 事件）；0 = 已检查上游响应但没有；NULL = 未记录（历史行、拿到上游响应前失败、非官方路径） |
| `turn_state_echo` | VARCHAR(16) DEFAULT '' | 入站回带的分类：`none`（客户端没带）/ `same` / `cross` / `unknown` / `substitute`（本会话替身，已换回真实值）；'' = 未记录 |
| `turn_state_stripped` | BOOLEAN DEFAULT FALSE | 本次入站值是否被网关剥离（cross / unknown / 无法还原的替身） |

- 采集点：出站前 `applyCodexTurnStateEchoPolicy` 的返回结果写入请求上下文（每请求一次，failover 不覆盖首个判定）；上游返回时 `relayCodexTurnStateResponseHeader` / `commitResponsesStreamAttempt` / `vaultCodexTurnStateEvent` 记录真实 token 长度到**当次尝试**的上下文槽位，落库时取胜出尝试的值（旧流迟到事件不污染新尝试）；未拿到上游响应的尝试记 NULL。统一在 `logUsageForRequest` 的填充链里补进 `UsageLogInput`（与窗口号同法），不逐个字面量手写。
- 筛选：`GET /api/admin/usage/logs` 与导出共用 `turn_state=received|missing|not_recorded`、`turn_state_length=N`、`turn_state_echo=<class>`、`turn_state_stripped=true|false`；在数据库分页前过滤；不回填旧日志。
- UI：用量列表模型列右侧新增「Turn-State」小列：`292 字符` / `未获取` / `未记录`，悬停显示回带分类与是否剥离，点击即按该状态筛选；「更多筛选」加 turn-state 状态、回带分类、是否剥离三个 `Select`；导出列同步。
- 账号页：健康状态条下方一行「最近 turn-state：292 字符 / 未获取 / 未记录 · 回带 cross（已剥离）」，来自按账号取最近一条终端用户请求的批量查询（PostgreSQL `unnest + LATERAL LIMIT 1`，SQLite 相关子查询），随账号列表分页只查当页账号。
- 三语文案；索引：`usage_logs(account_id, created_at DESC)` 已有则复用，否则不新建。

## 3. 首响应计时头（不再提前提交流）

- `codex_preflight_sse_passthrough_enabled` 保留键名与存储，语义改为「向 NewAPI 上报宽松首响应计时」：开启时**不再**提前透传前置元数据 / 提前 200；胜出尝试在正常提交响应头时附带：`X-Codex2API-Response-Timing: v1-loose`、`X-Codex2API-First-Response-Ms`（handler 入口到首个合格事件，含准入与此前重试）、`X-Codex2API-Attempt-First-Response-Ms`（本次尝试起算）。合格事件按宽松首字分类器（排除 lifecycle / error / terminal / 心跳）。
- 只在官方 `/v1/responses` 路径（HTTP 上游或 WS→HTTP 桥）生效；中转路径与原生 WS 客户端不发；已因心跳提前提交响应头时不上报；失败的缓冲尝试不发布计时；`response.failed` 前置错误重新获得静默换号/透明重试能力（DS P0-1 的收益）。
- 设置页文案改为「向 NewAPI 上报宽松首响应（不再提前提交 200）」+ 说明；三语。
- 计时只是响应头，不改变内容检测、重试资格、turn-state 托管与状态码。NewAPI 侧解析这些头另起任务。

## 不做的事

可编辑内置 prompt 规则；access-programs 诊断；turn-state 别名落库/校验诊断（我们已有托管）；回填历史用量日志。

## 测试

- Go：容量降载 500 不计连击、真 500 计数、显示 enabled 反映设置；turn-state 列在 HTTP/WS 官方路径、中转路径（NULL）、失败尝试（NULL）、迟到事件不污染；筛选 SQL（PG + SQLite）；账号最近 turn-state 批量查询；计时头：合格事件才记、心跳不记、失败尝试不发布、提前提交时省略、中转不发。
- 前端：守卫测试断言新列/筛选/文案；`npm test && npm run typecheck`。
- 上线后：用量页按 `turn_state_echo=cross` 或 `turn_state_stripped=true` 按账号看分布，即"降智账号"是否与外来 turn-state 相关的直接读数。
