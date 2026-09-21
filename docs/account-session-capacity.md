# 账号活跃会话容量

Codex 官方账号和 OpenAI Responses API 中转账号均可在账号编辑的调度设置中启用“账号活跃会话容量”，填写最大活跃会话数和空闲释放时间，保存后生效。批量编辑使用相同字段；默认关闭。

管理接口 `PATCH /api/admin/accounts/:id/scheduler` 与批量更新支持：

- `session_capacity_enabled`：是否启用。
- `session_capacity_max`：最大活跃会话数，1–100000。
- `session_capacity_idle_ttl_seconds`：空闲释放时间，60–2592000 秒。
- `session_capacity_reserved`：从总容量中预留的扩容专属窗口，默认 0。API-only 路由保持原有普通窗口策略，通常应保持为 0。

启用后已有会话可继续并刷新空闲时间；账号满额时新会话尝试其他可用账号，无可用账号则拒绝。已识别的关联子请求不重复占根窗口，明确免统计的被动请求保持免统计。无可靠会话身份的请求沿用原有不计窗口规则。

API 中转的用户窗口预授权豁免与账号容量是两个独立机制：API 请求仍可沿用原有用户预授权策略，但不能因此绕过管理员明确配置的账号容量。账号列表、详情和活跃窗口弹窗显示同一套运行时统计；关闭容量限制会释放该账号的已计窗口。

API 上游基础地址支持 HTTP 和 HTTPS。`CODEX_TRANSPORT_MODE=utls_chrome` 时，HTTP 请求使用普通 HTTP 传输，HTTPS 请求继续使用 Chrome TLS 指纹及证书校验；两条路径均沿用账号或请求选择的代理。
