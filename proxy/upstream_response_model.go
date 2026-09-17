package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// 上游自报模型：只观测上游响应信封里「上游自己声明的模型」，用于在用量页核对
// 上游是否把请求模型偷换成了别的模型。它纯粹是展示/诊断字段——不参与计费、
// 不参与调度、不回写请求，也绝不从请求体反推：请求模型已经单独记在
// model / effective_model 两列里，这里再填一遍就失去了对照意义。
//
// 观测点只取 response.model 与顶层 model 两个位置，绝不下钻到正文、工具输出
// 或协议翻译器合成的字段（那些值是网关自己造的，不是上游声明）。
const upstreamResponseModelMaxLen = 100

// upstreamResponseModelTerminalEvents 是会「覆盖」已观测值的终态事件。
// 非终态事件按先到先得：response.created 早于一切，最能代表上游接单时的模型；
// 终态则是上游最后的自我声明，中途换模型（如降级/回落）时以它为准。
func isUpstreamResponseModelTerminalEvent(event string) bool {
	switch event {
	case "response.completed", "response.done", "response.failed",
		"response.incomplete", "response.cancelled", "response.canceled":
		return true
	}
	return false
}

// observeUpstreamResponseModel 返回观测后的模型名：current 为已累积值，payload 为
// 一帧 SSE 事件数据或一份非流式响应体，eventType 为事件名（空则退回载荷里的 type）。
// 任何不合规的取值都保持 current 不变，绝不返回半截或被污染的字符串。
func observeUpstreamResponseModel(current string, payload []byte, eventType string) string {
	if len(payload) == 0 {
		return current
	}
	// 先验 JSON 合法性：gjson 对残缺 JSON 也会尽力取值，截断的帧可能取出半个模型名。
	if !gjson.ValidBytes(payload) {
		return current
	}
	model := ""
	for _, path := range []string{"response.model", "model"} {
		value := gjson.GetBytes(payload, path)
		if value.Type != gjson.String {
			continue
		}
		if model = strings.TrimSpace(value.String()); model != "" {
			break
		}
	}
	if !validUpstreamResponseModel(model) {
		return current
	}
	if strings.TrimSpace(eventType) == "" {
		eventType = gjson.GetBytes(payload, "type").String()
	}
	if current == "" || isUpstreamResponseModelTerminalEvent(strings.TrimSpace(eventType)) {
		return model
	}
	return current
}

// validUpstreamResponseModel 把上游值限制成「模型标识符」的形状：这个字符串会原样
// 落库并显示在管理端，不能让上游借它注入控制字符、凭据或整段响应正文。
func validUpstreamResponseModel(model string) bool {
	if model == "" || len(model) > upstreamResponseModelMaxLen {
		return false
	}
	// 误把凭据当模型名记下来会把密钥写进日志表并显示在页面上。
	if strings.HasPrefix(model, "sk-") || strings.HasPrefix(model, "eyJ") {
		return false
	}
	for _, char := range model {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case strings.ContainsRune("-_.:/", char):
		default:
			return false
		}
	}
	return true
}
