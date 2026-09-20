package proxy

import (
	"log"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/codex2api/database"
)

// 上游自报模型：只观测上游响应信封里「上游自己声明的模型」，用于在用量页核对
// 上游是否把请求模型偷换成了别的模型。它纯粹是展示/诊断字段——不参与计费、
// 不参与调度、不回写请求，也绝不从请求体反推：请求模型已经单独记在
// model / effective_model 两列里，这里再填一遍就失去了对照意义。
//
// 观测点只取三个协议各自的信封位置——Responses 的 response.model、Anthropic
// Messages 的 message.model、Chat Completions 与非流式整体响应的顶层 model，
// 绝不下钻到正文、工具输出或协议翻译器合成的字段（那些值是网关自己造的，
// 不是上游声明）。
const upstreamResponseModelMaxLen = 100

// upstreamResponseModelPaths 是信封里可能出现「上游自报模型」的位置，按可信度排序：
// 协议专属的嵌套位置优先于顶层 model。Anthropic 非流式响应体两处都有（顶层 model
// 与 type:"message"），顶层兜底即可；message_start 帧则只有 message.model。
var upstreamResponseModelPaths = []string{"response.model", "message.model", "model"}

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

// isUpstreamResponseModelEnvelopeEvent 是唯一可能携带模型信封的事件集合。
// 其余事件（delta / output_item.done / content_block_* / codex.rate_limits ...）
// 按协议就不带模型名，解析它们只是白跑。
//
// Responses 协议里带完整 response 对象的只有生命周期事件：created / queued
// （后台模式的开场帧）/ in_progress 与下面那组终态；Anthropic Messages 协议里带
// message 对象的只有 message_start 一个流式事件，外加非流式整体响应体自带的
// type:"message"——不放行后者，整份响应体会在解析前就被闸门挡掉。
func isUpstreamResponseModelEnvelopeEvent(event string) bool {
	switch event {
	case "response.created", "response.queued", "response.in_progress",
		"message_start", "message":
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
	for _, path := range upstreamResponseModelPaths {
		value := gjson.GetBytes(payload, path)
		if value.Type != gjson.String {
			continue
		}
		// 嵌套位置被污染时继续看下一个候选：这几处是同一个声明的不同写法，
		// 前者不合规不等于后者也不可信，直接放弃会白丢一条可用观测。
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

// 上游响应模型观测（移植自 sub2api 的 upstream response model audit）：
// 记录一次转发尝试内上游响应自报的模型名，与实际发往上游的模型对比，写入用量日志，
// 供管理端「模型不一致」审计与筛选使用。观测绝不影响转发与计费路径。
//
//   - first：首个声明（如 response.created 帧携带的 response.model）
//   - terminal：终态事件声明（response.completed / response.incomplete / response.failed），
//     存在时优先生效
//   - conflict：同一次尝试内出现互相矛盾的自报——上游混流或中途换模型的信号
type upstreamResponseModelObserver struct {
	first    string
	terminal string
	conflict bool
}

func (o *upstreamResponseModelObserver) Observe(model string, terminal bool) {
	model = normalizeObservedUpstreamResponseModel(model)
	if model == "" {
		return
	}
	if current := o.Model(); current != "" && !strings.EqualFold(current, model) {
		o.conflict = true
	}
	if terminal {
		o.terminal = model
		return
	}
	if o.first == "" {
		o.first = model
	}
}

// Model 返回本次尝试观测到的上游自报模型：终态声明优先，否则取首个声明；
// 上游从未自报时为空串。
func (o *upstreamResponseModelObserver) Model() string {
	if o == nil {
		return ""
	}
	if o.terminal != "" {
		return o.terminal
	}
	return o.first
}

func (o *upstreamResponseModelObserver) Conflict() bool {
	return o != nil && o.conflict
}

func normalizeObservedUpstreamResponseModel(model string) string {
	model = strings.TrimSpace(model)
	if !validUpstreamResponseModel(model) {
		return ""
	}
	return model
}

// observeUpstreamResponseModelFrame applies the same envelope validation to
// parsed SSE frames; lifecycle terminal declarations take precedence.
func observeUpstreamResponseModelFrame(o *upstreamResponseModelObserver, parsed gjson.Result, eventType string) {
	observeUpstreamResponseModelPayload(o, []byte(parsed.Raw), eventType)
}

// observeUpstreamResponseModelBody 供一次性响应体（非流式 JSON / compact 聚合结果）
// 调用：整个 body 就是终态。
func observeUpstreamResponseModelBody(o *upstreamResponseModelObserver, body []byte) {
	if o == nil {
		return
	}
	o.Observe(observeUpstreamResponseModel("", body, ""), true)
}

// Observe the original upstream envelope before any protocol translation, vault
// rewriting or synthesized terminal event. The shared parser retains all three
// protocols' envelope paths, event gating and credential/identifier validation.
func observeUpstreamResponseModelPayload(o *upstreamResponseModelObserver, payload []byte, eventType string) {
	if o == nil {
		return
	}
	model := observeUpstreamResponseModel("", payload, eventType)
	if model == "" {
		return
	}
	if strings.TrimSpace(eventType) == "" {
		eventType = gjson.GetBytes(payload, "type").String()
	}
	o.Observe(model, isUpstreamResponseModelTerminalEvent(strings.TrimSpace(eventType)))
}

// upstreamModelMismatch 三态判定：上游未自报 → nil（不参与筛选与展示，历史行同为
// NULL）；自报了 → 与实发模型大小写不敏感地严格比对。变体级别的宽松归一化（-latest /
// 日期后缀）不在此处做——那是前端徽章分级的展示语义，落库值保持原始对比结果。
func upstreamModelMismatch(sentModel, responseModel string) *bool {
	responseModel = strings.TrimSpace(responseModel)
	if responseModel == "" {
		return nil
	}
	sentModel = strings.TrimSpace(sentModel)
	mismatch := sentModel == "" || !strings.EqualFold(sentModel, responseModel)
	return &mismatch
}

// upstreamSentModelForAudit 返回审计对比用的实发模型：优先 attempt 实发
// （账号级映射可能改写），兜底客户端请求模型。
func upstreamSentModelForAudit(attemptModel, logModel string) string {
	if m := strings.TrimSpace(attemptModel); m != "" {
		return m
	}
	return strings.TrimSpace(logModel)
}

// applyUpstreamResponseModelObservation 把观测结果写入用量日志输入。上游未自报时
// 两个字段保持零值/nil。conflict 打一条告警（对齐 sub2api 的
// upstream_response_model_conflict），落库值取终态优先的 Model()。
func applyUpstreamResponseModelObservation(input *database.UsageLogInput, o *upstreamResponseModelObserver, sentModel string, accountID int64) {
	if input == nil || o == nil {
		return
	}
	model := o.Model()
	if model == "" {
		return
	}
	if o.Conflict() {
		log.Printf("upstream_response_model_conflict (account %d, endpoint %s): sent_model=%s, selected_response_model=%s", accountID, input.Endpoint, sentModel, model)
	}
	input.UpstreamResponseModel = model
	input.UpstreamModelMismatch = upstreamModelMismatch(sentModel, model)
}

// finalizeUpstreamResponseModelAudit covers usage branches that carry a captured
// model directly (including hidden continuation rounds). Preserve a comparison
// already made with the exact attempt model. Run after synthesized declarations
// are removed so they cannot leave behind a false audit result.
func finalizeUpstreamResponseModelAudit(input *database.UsageLogInput) {
	if input == nil {
		return
	}
	if input.UpstreamResponseModel == "" {
		input.UpstreamModelMismatch = nil
		return
	}
	if input.UpstreamModelMismatch == nil {
		input.UpstreamModelMismatch = upstreamModelMismatch(upstreamSentModelForAudit(input.EffectiveModel, input.Model), input.UpstreamResponseModel)
	}
}
