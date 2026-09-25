# 默认思考强度

Codex 原生 HTTP / WebSocket 及 BPS 推理请求没有提供思考强度时，使用 `low`。显式提供的 `none`、`minimal`、`low`、`medium`、`high` 等值继续遵循原有兼容规则；配置的模型别名和请求体规则仍然优先。Anthropic Messages 转到 Codex 时，没有 `output_config.effort` 也默认 `low`，Grok 自身的轮次策略不变。

默认值在 Codex 出站阶段补齐，不修改原始入站请求，不影响 API 账号的原样透传，也不向专用 `/responses/compact` 接口新增思考字段。

实际出站的强度记录在 `upstream.reasoning_effort` 和用量日志的思考强度列。BPS 的 `requested_reasoning_effort` 保留投影前提供的值；未提供时为空，`sent_reasoning_effort` 为 `low`，`adapted_fields` 标记默认补全。历史日志不会重新计算。
