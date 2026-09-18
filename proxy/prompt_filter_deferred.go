package proxy

import (
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// pendingPromptBlockContextKey 存放「先判后拦」的中间态。值类型 *pendingPromptBlock,
// 豁免执行后置为 nil。
const pendingPromptBlockContextKey = "prompt_filter_pending_block"

// promptFilterSourceAccountExempt 是账号级豁免放行的审计来源。与 local_filter 区分开,
// 后台才能把「本该拦住但因账号策略放行」的请求单独筛出来复核。
const promptFilterSourceAccountExempt = "account_exempt"

var (
	promptPolicyExempted              atomic.Uint64
	promptPolicyBlockedAfterSelection atomic.Uint64
)

// pendingPromptBlock 是「先判后拦」的中间态：选号前已判定为拦截，但要等选到账号后再决定
// 是否真的拦——落到 prompt_filter_policy=exempt 的账号时放行并审计。
type pendingPromptBlock struct {
	evaluation promptGuardEvaluation
	cfg        promptfilter.Config
	rawBody    []byte
	signedBody []byte
	endpoint   string
	model      string
	writeBlock func(*gin.Context, string)
	executed   bool
	blocked    bool
	delegated  bool
}

// PromptPolicyCounters 供运行状态面板读取:进程内计数,重启清零。
func PromptPolicyCounters() (exempted, blockedAfterSelection uint64) {
	return promptPolicyExempted.Load(), promptPolicyBlockedAfterSelection.Load()
}

func resetPromptPolicyCountersForTest() {
	promptPolicyExempted.Store(0)
	promptPolicyBlockedAfterSelection.Store(0)
}

// clearPendingPromptBlock 清掉上一轮遗留的中间态。一条 WebSocket 连接的每一轮都
// 复用同一个 gin.Context:判过但没执行的那一轮如果不清,后面一条干净的请求会在
// enforcePendingPromptBlockWS 里重放上一轮的拦截。
func clearPendingPromptBlock(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(pendingPromptBlockContextKey, nil)
}

func pendingPromptBlockFromContext(c *gin.Context) *pendingPromptBlock {
	if c == nil {
		return nil
	}
	raw, ok := c.Get(pendingPromptBlockContextKey)
	if !ok || raw == nil {
		return nil
	}
	pending, ok := raw.(*pendingPromptBlock)
	if !ok {
		return nil
	}
	return pending
}

// inspectPromptFilterOpenAIDeferred 与 inspectPromptFilterOpenAI 判定完全相同,但
// 不在选号前执行拦截副作用(锁会话、NewAPI 决策、写响应)。返回 true 只发生在两处
// 立刻硬拒上:必需的 NewAPI 身份缺失、会话已被锁定。
func (h *Handler) inspectPromptFilterOpenAIDeferred(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if c != nil && c.GetBool("prompt_intelligence_internal") {
		return false
	}
	if h == nil || h.store == nil {
		return false
	}
	pending, stop := h.evaluatePromptFilterHTTP(c, rawBody, endpoint, model, h.rejectRequiredNewAPIIdentity)
	if stop {
		clearPendingPromptBlock(c)
		return true
	}
	if pending == nil {
		clearPendingPromptBlock(c)
		return false
	}
	c.Set(pendingPromptBlockContextKey, pending)
	return false
}

// inspectPromptFilterAnthropicDeferred 是 /v1/messages 的延后变体,拦截响应仍然走
// Anthropic 自己的错误信封。
func (h *Handler) inspectPromptFilterAnthropicDeferred(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	pending, stop := h.evaluatePromptFilterHTTP(c, rawBody, endpoint, model, h.rejectRequiredAnthropicNewAPIIdentity)
	if stop {
		clearPendingPromptBlock(c)
		return true
	}
	if pending == nil {
		clearPendingPromptBlock(c)
		return false
	}
	pending.writeBlock = writeAnthropicPromptBlock
	c.Set(pendingPromptBlockContextKey, pending)
	return false
}

// inspectPromptFilterOpenAIForWebSocketDeferred 是 WS 入口的延后变体。已锁定的会话
// 仍然立刻拒(返回 blocked=true),本地 block 只登记待执行状态。
func (h *Handler) inspectPromptFilterOpenAIForWebSocketDeferred(c *gin.Context, conn *websocket.Conn, rawBody []byte, endpoint string, model string, policyEventID string) (blocked bool, delegatedToNewAPI bool) {
	if h == nil || h.store == nil {
		return false, false
	}
	pending, blocked, delegated := h.evaluatePromptFilterWS(c, conn, rawBody, endpoint, model, policyEventID)
	if blocked {
		clearPendingPromptBlock(c)
		return true, delegated
	}
	if pending == nil {
		clearPendingPromptBlock(c)
		return false, false
	}
	if c != nil {
		c.Set(pendingPromptBlockContextKey, pending)
	}
	return false, false
}

// enforcePendingPromptBlock 在选号后执行一次：豁免账号放行（审计 source=account_exempt），
// 其余账号写与旧路径逐字节相同的拦截响应。返回 true 表示请求必须在此结束（调用方负责 Release/Unbind）。
func (h *Handler) enforcePendingPromptBlock(c *gin.Context, account *auth.Account) bool {
	pending := pendingPromptBlockFromContext(c)
	if pending == nil {
		return false
	}
	if pending.executed {
		return pending.blocked
	}
	pending.executed = true
	if h.waivePendingPromptBlock(c, pending, account) {
		return false
	}
	promptPolicyBlockedAfterSelection.Add(1)
	pending.blocked = true
	return h.executePromptBlockHTTP(c, pending, pending.writeBlock)
}

// enforcePendingPromptBlockWS 是 WS 版本;豁免与计数语义和 HTTP 版本完全一致,
// 只是拦截时写的是 WS 错误帧。
func (h *Handler) enforcePendingPromptBlockWS(c *gin.Context, conn *websocket.Conn, account *auth.Account, policyEventID string) (blocked bool, delegatedToNewAPI bool) {
	pending := pendingPromptBlockFromContext(c)
	if pending == nil {
		return false, false
	}
	if pending.executed {
		return pending.blocked, pending.delegated
	}
	pending.executed = true
	if h.waivePendingPromptBlock(c, pending, account) {
		return false, false
	}
	promptPolicyBlockedAfterSelection.Add(1)
	pending.blocked = true
	blocked, delegated := h.executePromptBlockWS(c, conn, pending, policyEventID)
	pending.blocked = blocked
	pending.delegated = delegated
	return blocked, delegated
}

// abortIfPromptBlockPending 用在「判」与「拦」之间的每一条早退分支上:请求还没选号
// 就要结束时,先把待执行的拦截按原样写出去。拦截优先于校验/配额的拒绝——否则客户端
// 只要在命中的请求里多带一个非法字段(例如 tools[0].name=""),就能让每一次命中都
// 不写会话锁、不给 NewAPI 下发决策,只留一条审计。
//
// 账号传 nil 与「选不到账号」同义:这时还没选号,没有任何账号能豁免。代价是
// 畸形请求在豁免账号上也会被拦——可接受:豁免的是内容策略,不是请求合法性。
func (h *Handler) abortIfPromptBlockPending(c *gin.Context) bool {
	return h.enforcePendingPromptBlock(c, nil)
}

// abortIfPromptBlockPendingWS 是 WS 版本,语义与 abortIfPromptBlockPending 一致。
func (h *Handler) abortIfPromptBlockPendingWS(c *gin.Context, conn *websocket.Conn, policyEventID string) (blocked bool, delegatedToNewAPI bool) {
	return h.enforcePendingPromptBlockWS(c, conn, nil, policyEventID)
}

// waivePendingPromptBlock 处理账号级豁免:放行、计数、审计,并清掉中间态,
// 使同一请求后续再调用 enforce* 时不会重复审计。
func (h *Handler) waivePendingPromptBlock(c *gin.Context, pending *pendingPromptBlock, account *auth.Account) bool {
	if account == nil || !account.PromptFilterExempt() {
		return false
	}
	promptPolicyExempted.Add(1)
	h.logPromptFilterExemption(c, pending, account)
	clearPendingPromptBlock(c)
	return true
}

func (h *Handler) logPromptFilterExemption(c *gin.Context, pending *pendingPromptBlock, account *auth.Account) {
	h.logPromptFilterVerdictWithDecisionAndAccount(
		c, pending.endpoint, pending.model, promptFilterSourceAccountExempt, "",
		pending.evaluation.Verdict, &pending.evaluation.Decision, &pending.evaluation.Envelope, account.ID(),
	)
}
