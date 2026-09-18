# 会话防护（Session Guards）

以下开关（除 turn-state 托管默认开启外，其余默认关闭），用于验证「跨账号 turn-state / 上下文回带导致上游按身份降载」这一假设。全部热更新生效，进程内计数在「运维 → 运行状态 → 会话防护」查看，重启清零（自动锁定的锁落库，重启保留）。

| 设置 | 默认 | 作用 |
|---|---|---|
| 无（始终采集） | — | 每个 Codex 请求的入站 `X-Codex-Turn-State`（头或 `client_metadata.x-codex-turn-state`）按 same / cross / unknown 分类计数；cross、unknown 打 `[TURN-STATE]` 日志 |
| turn-state 严格模式 | 关 | 关：只剥离已知跨账号的回带（头 + 体）。开：来源未知的也剥离；上游 WS 握手不再带 token，改放每帧 client_metadata |
| 会话不借用账号 + 等待秒数 | 关 / 20 | 绑定账号并发满时先等它空出来（最多等待秒数），期内不借用其他账号；到期恢复原逻辑 |
| 首次会话 ID 年龄准入 + 最大年龄 | 关 / 180 | 无绑定的 Codex 原生会话按 UUIDv7 时间戳算年龄，超龄或未来时间拒绝 400 `codex_session_identity_unavailable`（`retry: stop`）。非 v7 只计数 |
| 连续 500 自动锁定会话 + 连续错误次数 | 关 / 3 | 同一会话（会话 ID + API Key）在官方 Codex 账号上连续最终 500 达阈值即锁定：400 `session_blacklisted`（`retry: stop`）。**只计真正的 `server_error`**：容量降载的 500（`server_is_overloaded`、`slow_down`，以及只在错误消息兜底判定里同义的 `service_unavailable_error`）既不 +1 也不清零，连击原样保留，等真正的 `server_error` 决定是否落锁。中转账号请求不计、`session_guards_policy=off` 的账号不计、内部重试不重复计、其余非 500 终态清零；锁落库、重启保留；解锁在「运行状态 → 已锁定会话」，接口 `GET/DELETE /api/admin/session-locks`。运行状态里的 `auto_lock` 开关、阈值与计数始终来自同一份运行设置，不再出现「显示 enabled=false 却仍在落锁」 |
| turn-state 托管 | 开 | 仅官方 Codex 账号；中转（relay）账号不托管，仍按第一轮规则处理。真实 `X-Codex-Turn-State` 只留在网关（1 小时），客户端拿随机替身（`c2a-ts-v1.…`，响应头与 `response.metadata` 事件都替换），回带时换回真实值；不是本会话当前替身的 token 一律剥离。带 `c2a-ts-v1.` 前缀却换不回真实值的回带（关掉托管后仍在手的旧替身、中转会话的替身）一律剥离，不受本开关与严格模式影响——网关自造的标记绝不出网关 |

## 建议的验证顺序

1. 只看观测 24h：记录被降载账号（`error_message LIKE '%server_is_overloaded%'` 按账号占比）与「外来 token 最多的账号」是否重合，以及 `borrowed` 次数。
2. 打开严格模式 24h，对比降载占比。
3. 打开不借用（等待 20s），观察 `held` 与请求排队延迟，再对比降载占比。
4. 最后再考虑首次会话准入：先看 `expired / future` 计数是否有误伤，再决定是否长期开启。

## 已知代价

- 不借用会在高峰期增加等待（最多等待秒数），等待秒数设为 30 时不再借用，超时直接返回无可用账号。
- 首次会话准入依赖粘性绑定：绑定 TTL 1 小时；没有 Redis 时重启会丢失绑定。开启后以下正常场景会被拒（都表现为 400 `codex_session_identity_unavailable`）：闲置超过 1 小时后继续的旧会话；`codex resume` / SDK `resumeThread` / VS Code 重新打开旧线程；`codex` 启动后超过最大年龄才发出第一句（线程 ID 在启动时生成）；客户端时钟比网关快 30 秒以上（每个新会话都判为"未来时间"）。运行状态页的 `expired / future` 计数就是这些误伤的直接读数，先观察再决定是否长期开启。
- 计数与 turn-state 精确溯源都是进程内存，多实例部署不汇总。
- 网关自己铸造的出站会话 ID（指纹收敛/隔离模式）是固定时间戳的 UUIDv7，因此不支持 codex2api 串联部署时开启首次会话准入：下游 codex2api 发来的每个首轮都会被判为超龄。
- 托管是进程内存：多实例部署下另一实例无法还原替身，只损失该轮粘性路由；重启后客户端下一轮拿到新替身。
- 自动锁定的连击只在内存，重启或保存设置清零；锁表跨实例只在对方重启后可见。
- 自动锁定的容量降载豁免跟随全局逃生阀：`CODEX_DISABLE_CAPACITY_SHED_HANDLING=1` 时降载重新按普通 500 计连击，与重试 / 亲和 / 冷却侧的回退保持一致。
- 锁表在进程启动后由首个请求触发异步加载，加载完成前（通常不到一秒）不拦截；加载失败后 30 秒再试。
- 首次会话准入现在在选号之后判定，落到中转账号的会话不再受影响。

## 账号级策略

账号管理 → 调度设置里每个账号有三个策略，默认都「继承全局」：

| 字段 | 取值 | 作用 |
|---|---|---|
| `prompt_filter_policy` | inherit / exempt | exempt：prompt 检测仍在选号前评估，但命中的请求落到该账号时放行；审计 source=`account_exempt` 并记录账号 ID。落到其他账号照拦（响应与原来完全一致）。每请求只判一次，failover 不重判。请求同时未通过参数校验时，待放行的拦截仍然生效（拦截优先于校验的提前返回）；这种情况下选号尚未发生，审计只有评估阶段的 source=`local_filter` 行（account_id 为 0），不会有 `account_exempt` 行。 |
| `egress_policy` | inherit / direct | direct 且未填固定代理：直连上游，不进代理池/全局代理/Resin；填了固定代理仍走固定代理 |
| `session_guards_policy` | inherit / off | off：该账号不做 turn-state 分类剥离与托管、不计 500 连击、不做首次会话准入与不借用、不记窗口号；网关铸造的替身仍不会被转发到上游。把正在使用中的账号切到 off 会让客户端手上那一轮的替身作废（该轮丢失一次粘性），下一轮恢复正常 |

代理探测现在会记录出口 IP 的时区（`test_timezone`，来自 ip-api 的 timezone 字段；IPv4 探测不通而走 IPv6 回退时暂不记录，只是不提示，不会提示错）；账号绑定的代理时区与账号时区不一致时，编辑页与列表会提示并可一键同步，不会自动改。代理池匹配保持按原文精确比较（末尾斜杠在后端是另一个 key）；账号编辑页会回显命中的池条目（标签 · 出口 IP · 地点 · 延迟），不在池中时明确提示。

## 用量日志 turn-state 列

每条用量日志记录本次**胜出尝试**的 `X-Codex-Turn-State` 情况，用来按账号确认「降智账号」。只有官方 Codex 账号会填：中转（relay）账号与 `session_guards_policy=off` 的账号一律留空 / NULL（判据与窗口号同源）。

| 列 | 类型 | 含义 |
|---|---|---|
| `turn_state_length` | `INT NULL` | 上游首次返回的**真实** token 长度（托管换成替身之前；token 是 ASCII base64，字节数等于字符数）。`0` = 已检查上游响应但它没给；`NULL` = 未记录（历史行、这次尝试没拿到上游响应、非官方路径）。两者不能合并 |
| `turn_state_echo` | `VARCHAR(16) DEFAULT ''` | 入站回带分类：`none`（客户端没带）/ `same` / `cross` / `unknown` / `substitute`（本会话替身，已换回真实值）；`''` = 未记录。白名单之外的值归为未记录，不落自由文本 |
| `turn_state_stripped` | `BOOLEAN DEFAULT FALSE` | 本次入站回带是否被网关剥离 |

采集分两条时间线：入站分类在出站前只判一次，failover 换号不改写「客户端带了什么」这个事实（首个判定优先）；真实 token 长度按**尝试**隔离——attempt 1 拿到 292、attempt 2 连响应都没拿到时，这条日志记 NULL 而不是 292，旧流的迟到事件只写进它自己的槽位。同一次尝试里 HTTP 响应头与 `response.metadata` 事件两个载体都会汇报，已记到真实长度后不会被后到的「检查过但没有」覆盖回 0。重试行（`retry=true`）各记各的，正是「哪次换号之后拿不到 turn-state」的逐次读数；WebSocket 一条连接上的每一轮也各算各的。

### 筛选

`GET /api/admin/usage/logs` 与共用同一套参数的区间统计卡片、错误摘要都支持：

| 参数 | 取值 | 含义 |
|---|---|---|
| `turn_state` | `received` / `missing` / `not_recorded` | 长度 `> 0` / `= 0` / `IS NULL` 三态。刻意不 COALESCE：把 NULL 折成 0 等于把所有历史行说成「上游从没给过」 |
| `turn_state_length` | 非负整数 | 精确匹配长度，按账号对比 292 / 312 这类桶标记时用 |
| `turn_state_echo` | `none` / `same` / `cross` / `unknown` / `substitute` | 回带分类 |
| `turn_state_stripped` | `true` / `false` | 本次是否被剥离 |

取值非法一律 `400`，不静默忽略：忽略之后页面显示「0 条」，会被读成「没有这类请求」而不是「参数写错了」。四个条件全部在数据库分页之前生效，历史行不回填。

### 页面读数

- 用量列表模型列右侧的 Turn-State 小列：`292 字符` / `未获取` / `未记录`，悬停显示回带分类与是否剥离，点击即按该状态筛选。长度与回带都没记的行整列不渲染——满屏「未记录」会把真正有数字的几行盖掉，这类行改用筛选器看。
- 「更多筛选」里有 turn-state 状态、回带分类、是否剥离三个下拉。
- 账号页健康状态条下面一行「最近 turn-state：292 字符 · 回带 cross（已剥离） · 3 分钟前」，取该账号**最近一条**终端用户请求（`internal_reason` 为空），随账号列表分页只查当页账号；刻意不往回找最近一条非空值——往回翻会把已经坏掉的号显示成健康的。后端没给样本时整行不渲染。

### 怎么读

长度是账号级状态标记，不是单次请求的成败信号：按账号看 `turn_state_length`（或账号健康条下的「最近 turn-state」），同一时刻同一模型下长度不同的账号就处在不同的上游桶里。`turn_state_echo=cross` 与 `turn_state_stripped=true` 按账号的分布，是「降智是否与外来 turn-state 相关」的直接读数。

### 实测记录与解读（2026-09-18，本号池）

- 同一模型（gpt-6-astra）、同一探测请求：唯一健康的官方号 244 的真实 turn-state 长度为 292，其余 9 个官方号全部为 312，且请求本身都成功。长度是「账号×模型」级别的状态标记，不是逐请求成败标记（252 号请求 gpt-5.5 得 292、请求 gpt-6-astra 得 312）。
- 「降智」在本池的表现是模型替换：处于 312 的账号请求 gpt-6-astra 时，上游实际以 gpt-5.6-luna 应答（用量页模型列下的「上游自报」可直接看到）；sol / terra 按请求应答。
- 注入实验结论：同号同模型重放自己的 token 会被接受（轮次延续）；同号跨模型注入 292 不能解除 astra 的降智；跨账号注入会得到 `response.failed` `server_error`，且承载该 token 的会话随后持续失败（污染是会话级的，新会话不受影响）。**因此不要把任何账号的 token 重放 / 注入到别的轮或别的号**；正确用法是按账号看 `turn_state_length`（或账号健康条下的「最近 turn-state」）识别 312 号，并据此做账号×模型的路由 / 冷却（后续工作）。
