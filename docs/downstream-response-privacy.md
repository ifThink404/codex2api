# 下游响应隐私边界

2026-09-19 更新。适用范围：原生 Codex Responses 的 HTTP、SSE、compact 与客户端 WebSocket，以及共享的公开错误出口。

## 两层处理

`response_privacy_fields.go` 在上游响应进入处理器时统一遍历协议对象。会话、线程、账号、组织、设备、安装、窗口、追踪和凭证字段不对外透传。字段名兼容大小写、下划线、连字符和 camelCase。对象、数组、嵌套对象和 metadata 中编码后的 JSON 字符串使用同一策略；重复键按 JSON 对象的最后值统一解释，无法解析或超过深度限制的控制子树被丢弃。

所有层级的 `headers` 字典只允许 Turn-State、模型和 reasoning 统计相关头。Turn-State 使用已有别名映射，在错误对象中直接删除。正常响应的主响应 ID 先建立映射，嵌套引用仅允许使用已验证的映射；未知诊断引用不会获得新的续链权限。缺少原生映射上下文时，不返回原始响应 ID。

模型文本、工具参数、JSON schema 等业务内容不按同名字段全局改写。输出项仍保留 `id`、`call_id` 和不透明上下文，输出项的 metadata 会进行过滤。

业务边界由身份过滤和公开错误过滤共用：推理摘要、电脑/终端/补丁工具动作、代码及执行结果、文件搜索结果、工具返回的 output 均按其协议位置保留。即使其中的 JSON 文本包含 `account_id`、`session_id` 或 `error`，也不能当作网关身份或网关错误改写。metadata 内伪造相同的工具类型不获得此豁免，相邻输出项 metadata 仍会过滤。

`response_public_error.go` 在向客户端发送之前生成公开错误。HTTP JSON、SSE 失败事件、WS 事件、握手异常、重试耗尽与超时均使用固定的公开文案和可识别错误分类。原始错误正文、任意 error details、组织/追踪头不再拼入用户错误或关闭原因。

内部重试、账号冷却、计费和安全判定仍使用内部错误信息。容量不足、上下文超限、续链失效、空响应等错误保留机器可识别语义。安全拒绝及网络/生物安全提示从网关判断重新生成，不信任上游自称是网关诊断的任意 details。

WS 的“隐藏上游错误”开关控制是否使用统一友好提示，不能关闭隐私处理。关闭时显示经过过滤的错误类别。

## NewAPI 管理员的来源文字豁免

NewAPI 在已绑定渠道的签名策略元数据中，为启用状态的管理员和 Root 用户自动发送 `preserve_upstream_source: true`，身份以请求令牌所属用户为准。codex2api 仅在用户身份、正文摘要、策略元数据签名和 API Key 绑定验证通过后接受它。普通请求头、正文中同名字段、缺失或无效签名不能开启豁免。

豁免仅保留模型回答、代码、链接和工具参数中的上游来源名称及地址，覆盖 HTTP JSON、compact、SSE 分片及 Responses WebSocket 共享响应出口；原生 Codex 和 BPS 请求均适用。账号凭据、会话身份、Turn-State/response ID 映射、BPS 调用方工具投影及公开错误安全处理继续执行，不启用测连的原始响应模式。

此功能需要同时更新 NewAPI 和 codex2api，并启用两端已有的签名绑定。旧版 NewAPI 没有发送标记时继续执行原有替换；长连接需重新连接以取得新的签名身份。使用日志的 `diagnostics.incoming.signed_newapi.preserve_upstream_source` 记录实际生效值（字符串 `true` 或 `false`）。不增加客户端开关或依据客户端自报角色放行。

## 自助用量

公开 Key 用量接口使用独立的 `publicAPIKeyLimits` 结构，保留用户自己的预算、模型和功能限制；不再直接返回内部限制对象中的账号 scope ID 和分组 ID。后台配置不被修改。

## 账号测连

后台账号测连通过内部 `WithCodexAccountTestRawResponse` 上下文保留原始上游响应，跳过面向普通用户的身份清理。这样诊断记录可显示真实 Turn-State、响应 ID、请求标识及正文中的嵌套元数据；现有凭据脱敏、响应头白名单和正文预览大小限制继续生效。

此上下文只在测连入口设置，并绑定所选账号。请求头、请求正文及运行配置无法开启它；如果上下文已有普通用户的 Turn-State 或响应 ID 映射，仍然执行用户响应清理。该选项不改变出站请求的身份映射或 Turn-State 校验。

## 保持的边界

- API relay 的响应 ID 继续使用该供应商的续链协议，不纳入原生 Codex 的 ID 映射。
- `encrypted_content`、工具引用及模型生成的业务内容仍按协议传输；这不是对所有不透明内容进行重新加密的实现。
- 模型、usage、正常限额事件及必要的重试时间语义不伪造。不能把本修复描述为消除全部统计关联。
- Turn-State 的长度/公开时间形态，以及已有会话 UUID 映射策略没有迁移；协议身份不再因为本次发现的正文、嵌套和错误出口遗漏而回传。
- 管理诊断有独立权限，仍可用于排障；不要把管理端导出开放给普通用户。

## 回归验证

`response_privacy_fields_test.go` 覆盖身份字段、字段变体、深层 headers、JSON 字符串、数组、重复键、超深/畸形数据、嵌套引用无权限提升及业务内容保持。

`response_privacy_boundary_test.go` 与 `wsrelay/response_privacy_boundary_test.go` 使用本地模拟上游，验证 HTTP SSE、JSON、compact、真实上游/下游 WS、普通错误、停用错误及握手失败；关闭 WS 错误隐藏时也不得出现原值。

`response_privacy_compatibility_test.go` 另外验证业务内容保持、请求 input/tools 字节及大整数保持、首个 SSE 事件不等待 EOF。HTTP/SSE/compact/客户端 WS 边界同时断言推理摘要、工具参数、模型及 usage 保持不变。

自助字段投影由 `admin/key_usage_public_privacy_test.go` 验证。所有样例均为虚构标记，不需要真实账号或生产模型调用。

`admin/codex_test_raw_response_test.go` 将本地 HTTP/WS 上游、原生执行器和测连诊断串联，覆盖成功及失败响应，验证原始身份可诊断且凭据仍脱敏。`proxy/codex_account_test_response_test.go` 验证测连开关不能绕过普通用户响应隐私，也不放宽出站状态校验。
