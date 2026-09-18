package proxy

import (
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// upstream_response_model 这一列的全部意义是「上游自己声明了什么模型」，用来跟请求
// 模型对照，看上游有没有偷换。所以它有一条铁律：绝不从请求体反推。一旦记进去的是
// 网关自己知道的模型，两边就永远相等，真被偷换时反而看不出来——那比留空更糟，因为
// 留空是「没数据」，而回显是「有数据且显示正常」。
//
// Antigravity OAuth 账号恰好踩中这条线。它的响应不是上游原样回来的：
// ExecuteAntigravityResponsesRequest（proxy/antigravity_responses.go:126）把 Gemini
// 的回包整体改写成 Responses 信封，改写用的是
// newAntigravitySSEResponseBodyWithCustomTools / newAntigravityJSONResponseBodyWithCustomTools，
// 两者写进信封的 model 都是 publicModel —— 也就是网关这次请求用的模型；上游真正
// 自报的 modelVersion 在改写中被丢弃（全仓 grep modelVersion 无其它引用）。
// 观测点看到的是这份合成信封，于是记下来的就是请求模型的回显。
//
// 要记真值得让适配器把 modelVersion 单独带出来（例如仿 grokNativeRouteHeader 用一个
// 进程内响应头），那是一次独立的设计改动。在那之前，这里统一抹成空。
//
// Antigravity API Key 账号不受影响：它走 executeAntigravityInteractionsRequest
// （proxy/antigravity_responses.go:424），上游直接说 Responses，拿回的信封未经改写，
// 里面的 model 是货真价实的上游声明，照常记录。
func upstreamResponseModelIsGatewaySynthesized(account *auth.Account) bool {
	return account != nil && account.IsAntigravityAPI() &&
		account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth
}

// clearSynthesizedUpstreamResponseModel 在落库前抹掉网关自己合成的模型名。
//
// 放在 logUsage 这个总出口，而不是逐个观测点加判断：观测点散在六处读循环里，
// 漏掉一个就等于放行一条假声明，而这个判断只要账号、不要请求上下文。logUsage
// 又是所有写入路径（含不走 logUsageForRequest 的 logLiveUsage）的必经之地。
func (h *Handler) clearSynthesizedUpstreamResponseModel(input *database.UsageLogInput) {
	if h == nil || h.store == nil || input == nil {
		return
	}
	if input.UpstreamResponseModel == "" || input.AccountID <= 0 {
		return
	}
	if upstreamResponseModelIsGatewaySynthesized(h.store.FindByID(input.AccountID)) {
		input.UpstreamResponseModel = ""
	}
}
