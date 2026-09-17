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
- 首次会话准入依赖粘性绑定：绑定 TTL 1 小时；没有 Redis 时重启会丢失绑定。开启后以下正常场景会被拒（都表现为 400 `codex_session_identity_unavailable`）：闲置超过 1 小时后继续的旧会话；`codex resume` / SDK `resumeThread` / VS Code 重新打开旧线程；`codex` 启动后超过最大年龄才发出第一句（线程 ID 在启动时生成）；客户端时钟比网关快 30 秒以上（每个新会话都判为"未来时间"）。运行状态页的 `expired / future` 计数就是这些误伤的直接读数，先观察再决定是否长期开启。
- 计数与 turn-state 精确溯源都是进程内存，多实例部署不汇总。
