package proxy

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// Codex 客户端的窗口标识形如 <uuid>:<n>，最后一段是该客户端本地的窗口序号
// （同一台机器上开的第几个 Codex 窗口）。它同时出现在 X-Codex-Window-Id 请求头
// 与请求体 client_metadata.x-codex-window-id 两处。
//
// 这里记录的是**客户端原始值**：指纹收敛（ApplyCodexFingerprintToBody /
// ApplyCodexFingerprintHeaders）改写的是出站副本，raw_body 与入站请求头都还是
// 客户端发来的原值，所以在日志侧读这两处不会读到收敛后的号。
const codexWindowNumberMaxDigits = 20 // uint64 十进制最大位数

// parseCodexWindowNumber 取窗口号的十进制字符串；缺失或形状不对一律返回空串，
// 绝不猜测。请求体优先于请求头：WebSocket 的握手请求头属于整条连接，
// 只有帧体里的值才是这一轮的。
func parseCodexWindowNumber(header http.Header, body []byte) string {
	if number := codexWindowNumberFromID(codexWindowIDFromBody(body)); number != "" {
		return number
	}
	if header == nil {
		return ""
	}
	return codexWindowNumberFromID(header.Get(codexWindowIDHeader))
}

func codexWindowIDFromBody(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	value := gjson.GetBytes(body, "client_metadata.x-codex-window-id")
	if value.Type != gjson.String {
		return ""
	}
	return value.String()
}

// codexWindowNumberFromID 解析 <uuid>:<n>。冒号前必须有内容（真实客户端发的是
// thread uuid），冒号后必须是纯十进制且落在 uint64 内，否则视为畸形。
func codexWindowNumberFromID(windowID string) string {
	windowID = strings.TrimSpace(windowID)
	cut := strings.LastIndex(windowID, ":")
	if cut <= 0 || cut == len(windowID)-1 {
		return ""
	}
	digits := windowID[cut+1:]
	if len(digits) > codexWindowNumberMaxDigits {
		return ""
	}
	number, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return ""
	}
	// 归一化前导零，让同一个窗口在页面上不会出现 "07" / "7" 两种写法。
	return strconv.FormatUint(number, 10)
}

// windowNumberAppliesToAccount：窗口号只对官方 Codex OAuth 账号有意义。它是
// Codex 客户端的本地窗口序号，只有官方通道上的请求才保证由 Codex 客户端发出；
// relay / Grok / Antigravity / Claude 账号服务的客户端可以是任何东西，它们即使
// 带了 X-Codex-Window-Id，那串 <uuid>:<n> 也不具备同一套语义，记下来只会让用量页
// 把互不相干的窗口号并排显示。把 session_guards_policy 设成 off 的账号同理：运营者
// 已经宣布那号上的客户端不按 Codex 契约走，它的窗口号也就没有可比语义。
//
// 判据走 sessionGuardsActiveFor，与 turn-state 托管同源但不复用
// turnStateVaultAppliesTo：托管还受 CodexTurnStateVaultEnabled 开关控制，
// 而窗口号记录与那个开关无关。
func windowNumberAppliesToAccount(account *auth.Account) bool {
	return sessionGuardsActiveFor(account)
}

// populateUsageWindowNumber 在日志落库前补上窗口号。放在 logUsageForRequest 的
// 统一填充链里，而不是逐个 UsageLogInput 字面量里手写：漏掉一条就等于该路径的
// 窗口号永久缺失，而这条信息只有请求侧拿得到。
//
// 挂在 Handler 上是因为「是不是官方账号」只有号池答得出：UsageLogInput 只带
// AccountID，日志侧要回查一次（与 logUsage 固化 Channel 时同源的查法）。
func (h *Handler) populateUsageWindowNumber(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || input.WindowNumber != "" {
		return
	}
	// 账号无从判定时一律不记：宁可缺一个展示字段，也不把非 Codex 客户端的
	// 窗口号混进来。
	if h == nil || h.store == nil || input.AccountID <= 0 {
		return
	}
	if !windowNumberAppliesToAccount(h.store.FindByID(input.AccountID)) {
		return
	}
	var header http.Header
	// 只有非 WebSocket 请求才允许回落到请求头：Upgrade 请求的头在整条连接上复用，
	// 用它填每一帧会把第一帧的窗口号串到后续所有轮次上。
	if c.Request != nil && !isWebSocketUpgradeRequest(c.Request) {
		header = c.Request.Header
	}
	body, _ := rawRequestBodyFromContext(c)
	input.WindowNumber = parseCodexWindowNumber(header, body)
}

func isWebSocketUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
		}
	}
	return false
}
