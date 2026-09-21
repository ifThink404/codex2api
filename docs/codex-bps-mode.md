# 账号级 BPS 兼容模式

普通 Codex OAuth / AT 账号的编辑窗口提供“BPS 兼容模式”，默认关闭，点击保存后生效。配置键为 credentials.codex_bps_enabled，支持运行时同步、重新加载及原生账号 JSON 导入导出。API 中转和 Agent Identity 不支持开启。

## 会话与请求路径

- 新绑定的主会话把上游模式写入原有 session continuity 记录。既有记录未注明模式时按 native 处理。保存开关不会迁移已有会话；后台请求沿原有父会话绑定继承模式。
- 切号仍使用既有找号、容量和上下文规则，并排除另一种模式的候选账号。
- BPS 使用账号自己的 Access Token、官方 Account ID 和代理，HTTP/SSE 生成地址为 `https://bps.openai.com/basispoints/api/responses`；压缩地址追加 `/compact`。即使全局强制 WS，BPS 仍走 HTTP。不会在失败时自动回落到原生 Codex 路径。
- 单独测连和批量测连均先选择“按账号配置 / Codex / BPS”，再开始测试。选择仅作用于本次测试，不写账号配置，不改变业务会话绑定。显式选择不支持的路径会报错；不会自动换路。BPS 批量测连跳过原生 WHAM 预检，直接验证所选路径。
- 现有账号隐私处理先执行，再做 BPS 投影。BPS 设备头使用账号设备 ID；任务、轮次和缓存标识从处理后的标识派生，并使用独立命名空间。原生 Turn-State、客户端自定义握手头不转发到 BPS。

## 格式转换

| 内容 | BPS 行为 |
| --- | --- |
| `codex-auto-review` | 使用原模型名发送，不再映射为 `gpt-5.6-luna`；其他模型同样维持原选择 |
| `input`、历史工具调用和结果、加密历史 | 经过既有隐私处理后保留；不删除工具业务内容 |
| 顶层 `instructions` | 放入 developer 输入消息 |
| 顶层 `tools` | 放入 additional_tools 输入项；已有 additional_tools 保留 |
| `reasoning.effort` | 生成请求使用顶层 `reasoning_effort` |
| `client_metadata` / 顶层 `metadata` | 转为 BPS task_id、turn_id、工具版本与 agent_iteration；原身份元数据不透传 |
| 生成 | model、input、metadata、model_selection=explicit、stream=true、store=false、reasoning_effort、prompt_cache_key |
| 压缩 | 只发送 model、input、metadata；不发送 prompt_cache_key 等生成参数 |

BPS 是经过实测的兼容投影，服务端不接受的顶层控制字段（包括 service_tier、text、tool_choice、parallel_tool_calls、include 等）不发送。按用户选择，不增加 JSON Schema 校验、额外工具拦截或并行控制执行层；这些控制不保证等价支持。原始提示和工具定义仍保留。其他未进入上述投影的顶层字段同样记录为移除字段，方便排查。

## 日志

使用日志端点旁显示 BPS，实际地址记录为 BPS 地址，实际传输标记 HTTP。请求诊断及测连展示请求模型、实际发送模型、上游自报模型（仅上游提供时）、格式转换和移除的字段名。原始模型仍保留在请求诊断中。

本地出站快照记录 BPS 身份头和 metadata 的脱敏值；配置、诊断和转换记录不发送上游。BPS 返回的任务标识沿既有响应隐私边界处理，工具参数等业务内容不按身份元数据整体清理。

BPS 正常响应头未携带 Turn-State 时，向客户端补充本地模拟令牌：217 字节、292 字符、URL-safe Base64，带版本、时间戳、随机数据及本地 HMAC。同一用户/线程/轮次/账号/代次复用，换轮或换号后更换。模拟类型写入认证的持久化绑定，回传时只校验并清除，不恢复成上游状态，不作为找号或保活凭据；原生 Codex 路径不补模拟值。HTTP 头和流式 metadata 使用同一值。

日志保留独立的真实上游观察值与 `client_turn_state`（source=synthetic）。上游实际 0 字符不会被改写成 292；测连也分开显示。模拟令牌不代表缓存命中或模型能力改变。

账号批量编辑增加“修改 BPS 兼容模式”与“开启”两个独立控件；未选修改时不提交此字段。默认关闭，保存后生效。

## 公开响应中的地址隐藏

统一清理私有 BPS 域名（含大小写、URL 编码与 JSON Unicode 转义形式），使用等长 `hidden.invalid` 替代。真实连接目标、管理员本地诊断保留；客户端响应和测连输出不携带真实域名。覆盖 HTTP 头、JSON 正文/字段名/嵌套字符串、公开错误、SSE/WS 文本和工具参数增量。增量按输出项分别缓存潜在域名前缀，跨事件拼接处理，保留事件顺序；非目标文本及工具业务内容保持原样。

这是对明确域名及其已覆盖编码形式的过滤，不是对模型语义、任意编码或图片内容的推断过滤。
