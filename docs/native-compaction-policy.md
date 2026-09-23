# 账号原生远程压缩限制

账号编辑和批量编辑支持 `codex_native_compaction_only`，默认关闭，保存后对该账号的后续请求生效。该选项不改变上游请求路径，也不会自动调整客户端配置。

开启后，实际走非 BPS 路径的普通 Responses 请求如果通过当前轮次元数据声明压缩，却没有直接的 `input[].type=compaction_trigger`，网关会在出站前返回 HTTP 400，错误码 `native_compaction_required`，提示升级或调整客户端。原生 Codex 的 HTTP、WS 和 Responses API 中转路径执行该限制；专用 `/responses/compact` 仍按既有压缩协议处理。

实际走 BPS 的请求豁免此限制，继续允许旧式文本摘要压缩，即使账号同时开启了这两个选项。判断依据是本次请求最终选定的路径，包含已有会话绑定的模式和管理员测连模式，不能只看账号当前的 BPS 开关。此豁免不会插入或移除压缩触发器，也不会把原生压缩结果转换成普通摘要。

检查只针对控制元数据和直接输入项，不搜索用户正文、工具参数或工具结果中的“压缩”等文字。普通请求携带历史压缩项不会被拒绝。未声明压缩意图的普通文本请求无法据此识别为旧式摘要压缩。

新策略依赖客户端正确保存并在后续请求中携带压缩结果；仅在网关中开启该选项不能将旧客户端自动升级为原生压缩。上游也必须支持对应压缩协议。

## BPS 思考强度

BPS 普通请求会将 `reasoning.effort=max` 转成出站 `reasoning_effort=xhigh`。直接的 `configuration_update` 协议项也做相同调整；消息正文、工具参数和结果原文保留。原生 Codex 路径不做这项降级。

本地 BPS 兼容诊断增加 `requested_reasoning_effort`、`sent_reasoning_effort`，并在 `adapted_fields` 中记录映射。诊断字段不加入上游请求。专用压缩接口不额外添加其协议未声明的思考强度参数。
