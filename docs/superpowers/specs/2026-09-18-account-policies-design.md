# 账号级策略（prompt 豁免 / 出口直连 / 会话防护关闭）+ 代理时区同步 + 代理匹配回显 设计

日期：2026-09-18（第四轮；基于 v2.9.8-fr-20260918.3 之后的 `codex/production-main`）
前置：`docs/superpowers/specs/2026-09-17-codex-session-guards-design.md`、`docs/superpowers/specs/2026-09-17-session-auto-lock-design.md`

## 背景

用户的号池同时有官方 Codex OAuth 号与中转（`responses_api` / Grok / Antigravity / Claude）号。本周加的会话防护（托管、不借用、准入、连续 500 计数、窗口号）已只作用于官方号，但 **prompt 检测**与**出口代理池**仍对所有账号一视同仁：中转号的上游不是 OpenAI，既不需要 prompt 风控保护，也不需要出口代理。另外两个运维痛点：官方号绑定了独占代理但账号时区与出口 IP 所在时区不一致；账号编辑页看不出当前 proxy_url 对应代理池里的哪条。

## 1. 账号级策略字段

`accounts` 表新增三列（PostgreSQL / SQLite 各自 DDL，`New()` 启动时 ensure）：

| 列 | 类型 | 取值 | 默认 |
|---|---|---|---|
| `prompt_filter_policy` | VARCHAR(16) | `inherit` / `exempt` | `inherit` |
| `egress_policy` | VARCHAR(16) | `inherit` / `direct` | `inherit` |
| `session_guards_policy` | VARCHAR(16) | `inherit` / `off` | `inherit` |

- `auth.Account` 增加同名字段（字符串，规范化：空/未知 → `inherit`），随账号快照热更新（沿用 `ScoreBiasOverride` 的加载/更新路径与 outbox 同步）。
- 管理接口：`GET /api/admin/accounts` 每行返回三个字段；`PATCH /api/admin/accounts/:id/scheduler` 接受三个字段（`json.RawMessage`，缺省不改，非法值 400）。
- UI：账号管理 → 调度设置弹窗，紧跟「跳过 warm 层级」之后加三个 `Select`（继承全局 / 豁免、继承全局 / 直连、继承全局 / 关闭），每个带一行说明；账号列表行在代理徽章旁按策略显示小徽章（`豁免检测`、`直连`、`防护关`），仅非 inherit 时显示。三语文案。
- 任何账号类型都可设置；默认全部 inherit，上线不改变行为。

## 2. prompt 检测「先判后拦」

现状：`inspectPromptFilterOpenAI` / `…Anthropic` / `…OpenAIForWebSocket` 在选号之前评估并**直接写拦截响应**（Responses、compact、chat、WS、messages 各一处；images / Grok 媒体另有入口）。

改为两段式（仅 Responses、compact、chat、WS、messages 五个会落到中转 Codex 号的入口；images / Grok 媒体保持原样）：

1. **评估**（位置不变，选号前）：命中时不写响应，把 `pendingPromptBlock{decision, writeBlock func()}` 放进 gin context；未命中零开销。审计记录在评估阶段照旧生成但先不落库。
2. **执行**（选号后、出站前——与首次会话准入判定同一位置，HTTP 与 WS 各一处）：
   - 选中账号 `prompt_filter_policy == exempt` → 丢弃待拦截决定，审计落库并标记 `exempted_by_account=<id>`，请求继续；
   - 否则 → 调用 `writeBlock()` 写与现在完全相同的拦截响应，`h.store.Release(account)`，`UnbindSessionAffinity`，审计落库标记拦截；WS 写错误帧后按现有策略关闭。
   - 每请求只执行一次（`pendingPromptBlock` 带 `evaluated` 标记；failover 到别的账号不重判——决定按**第一个**选中账号生效）。
3. NewAPI 绑定的 `prompt_filter_scope`（inherit / local_only / off）语义不变，先于本机制生效（off 直接不评估）。
4. 观测：运行状态 → 会话防护面板加一行「prompt 豁免」计数（`exempted` / `blocked_after_selection`）。

## 3. 出口直连

`resolveProxyForAccountSnapshot` 最前面加规则：账号 `egress_policy == direct` 且 `proxy_url` 为空 → 返回 `("", true)`（直连、允许 direct），跳过组代理、代理池、全局代理与代理池 fail-closed（issue #517 的保护只对 inherit 账号继续生效）；`proxy_url` 非空时固定代理优先级不变。Resin：官方号且 direct 时同样不经 Resin（`resinCarriesEgress` 视为 false）。

## 4. 代理绑定的时区同步

- 代理探测（`runProxyProbe`）在现有 IP / 地点之外记录出口 IP 的 IANA 时区：探测服务返回则直接用；不返回则按国家 + 地区映射表推导（美国 / 加拿大 / 澳大利亚 / 俄罗斯 / 巴西按地区，其余按国家取首选时区；无法推导则留空）。落 `proxies.test_timezone`（VARCHAR(64)，默认空），代理列表接口透出。
- 账号编辑页与快速绑定弹窗：当输入框的 proxy_url 按第 5 节规则命中池条目、条目有 `test_timezone` 且与账号当前时区不同 → 在代理输入框下方显示黄色提示「出口 IP 时区 X，账号时区 Y」+「同步为 X」按钮；点击后立即调用 `PATCH /accounts/:id/scheduler {timezone: X}` 并刷新。保存代理绑定（快速弹窗）时若不一致，保存成功后同一提示以 toast + 按钮形式出现。
- 账号列表：OAuth 号当绑定代理时区与账号时区不一致时显示小徽章「时区不一致」，点击打开编辑页。
- 只提示、不自动改；无绑定代理或代理无时区的账号不提示。

## 5. 代理池匹配回显

- 前端新增 `normalizeProxyURLForMatch(url)`：小写 scheme 与 host、去掉末尾 `/`、保留端口与用户名、忽略密码与空白；`ProxyField` / `ProxyPoolSelect` / 账号列表代理徽章（`resolveAccountProxyBinding`）改用规范化比较。
- 命中时在输入框下方回显该条目的 标签 · 出口 IP · 地点 · 延迟；未命中显示「不在代理池中」（灰字）。
- 后端 `CountAccountsByProxyURL`（列表页 bound_count）改用同一规范化规则，避免徽章与计数不一致。

## 不做的事

按 API Key / 分组的 prompt 豁免；自动同步时区；把账号 proxy_url 改成外键；images / Grok 媒体入口的先判后拦；改动 prompt 检测规则本身。

## 测试

- Go：策略字段规范化与持久化（PG + SQLite）；先判后拦：exempt 账号放行、inherit 账号被拦（响应体与现在逐字节一致）、failover 不重判、WS 路径；出口直连解析（direct / direct+固定代理 / inherit + 池 fail-closed）；探测时区推导表；`CountAccountsByProxyURL` 规范化。
- 前端：`normalizeProxyURLForMatch` 单测；源码守卫测试断言三个 Select、时区提示、回显块与三语文案键。
- 上线后：默认 inherit，行为不变；对一个中转号打开「豁免」后用被拦截样例请求验证放行且审计有 `exempted_by_account`。
