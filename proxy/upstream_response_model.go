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

// isUpstreamResponseModelTerminalEvent 是会「覆盖」已观测值的终态事件。
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

// isUpstreamResponseModelEnvelopeEvent 是唯一可能携带 response 信封的事件集合。
// 其余事件（delta / output_item.done / codex.rate_limits ...）按协议就不带
// response.model，解析它们只是白跑。
func isUpstreamResponseModelEnvelopeEvent(event string) bool {
	switch event {
	case "response.created", "response.in_progress":
		return true
	}
	return isUpstreamResponseModelTerminalEvent(event)
}

// observeUpstreamResponseModel 返回观测后的模型名：current 为已累积值，payload 为
// 一帧 SSE 事件数据或一份非流式响应体，eventType 为事件名（空则退回载荷里的 type）。
// 任何不合规的取值都保持 current 不变，绝不返回半截或被污染的字符串。
func observeUpstreamResponseModel(current string, payload []byte, eventType string) string {
	if len(payload) == 0 {
		return current
	}
	// SSE 读循环是全系统最热的路径：一次串流几百上千帧，其中只有信封事件带模型。
	// 不设闸门的话每一帧都要先做一遍 ValidBytes 全量扫描，而 output_item.done 里
	// 的 reasoning encrypted_content 动辄 20KB——成本跟着正文体积涨，跟要取的那个
	// 模型名毫无关系。调用方没给事件名时（非流式整体 JSON）退回载荷里的 type，
	// 两者都没有才真的解析：非流式响应体本身不带 type 字段。
	event := strings.TrimSpace(eventType)
	if event == "" {
		event = strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	}
	if event != "" && !isUpstreamResponseModelEnvelopeEvent(event) {
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
		// 信封里的 response.model 被污染时继续看顶层 model：两个位置是同一个声明的
		// 两种写法，前者不合规不等于后者也不可信，直接放弃会白丢一条可用观测。
		if candidate := strings.TrimSpace(value.String()); validUpstreamResponseModel(candidate) {
			model = candidate
			break
		}
	}
	if model == "" {
		return current
	}
	if current == "" || isUpstreamResponseModelTerminalEvent(event) {
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
