# 首次主会话处理（sever）

`sever` 已移除首次主会话 ID 的 UUIDv7 格式校验和时间差准入校验。普通 SDK 的 UUIDv4、字符串会话 ID，以及过旧或超前的 UUIDv7，均不会再被这项检查拒绝。

旧配置 `codex_initial_session_max_age_seconds` 和 `codex_initial_session_age_check_disabled` 保留存储、导入和导出兼容性，但不再参与请求准入；导入旧配置也不会重新开启检查。设置页已撤下时间差开关和输入框，运行状态页已撤下相关统计。兼容的运行状态 API 始终返回 `initial_session.enabled: false`，统计为零。

首次选号仍标记 `initial_session.result: disabled`，并在每次出站边界清除旧 `X-Codex-Turn-State` 和 `client_metadata.x-codex-turn-state`，覆盖内部重试与 HTTP/WS 转换。不修改 input、tools 或其他历史，已绑定请求沿用原有处理。

账号绑定、会话序号模式、压缩原账号归属、NewAPI 签名验证和出站身份映射独立运行。本次不关闭这些机制；其他阶段仍可能返回同名身份错误，应结合日志 stage 定位。

此行为仅适用于 `sever`，不修改 `main`。
