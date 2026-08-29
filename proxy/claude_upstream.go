package proxy

// Claude Code(Anthropic)OAuth 账号的上游透传。
//
// 与其它 relay 账号不同:Grok / OpenAI-Responses 中转都会把请求翻译成 Codex
// "Responses" 协议再出站,而 Claude 账号本身就说 Anthropic Messages API,因此这里
// 采用近乎透传——把入站的原始 Anthropic body 直接发往 api.anthropic.com/v1/messages,
// 仅注入 OAuth 凭据要求的三件套:
//   - Authorization: Bearer <access_token>
//   - anthropic-beta: oauth-2025-04-20（与入站已声明的 beta 合并去重）
//   - system 数组首块必须是 "You are Claude Code, Anthropic's official CLI for Claude."
//     否则 Anthropic 会拒绝 OAuth token 的推理请求。
//
// 返回原始 *http.Response 交由调用方按 SSE 流式回传,响应本身已是 Anthropic 格式,
// 无需再做协议翻译。

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/text/unicode/norm"
)

const (
	// claudeMessagesEndpoint 是 Anthropic 官方 Messages API 端点。
	claudeMessagesEndpoint = "https://api.anthropic.com/v1/messages"
	// claudeAnthropicVersion 是 Messages API 版本头。
	claudeAnthropicVersion = "2023-06-01"
	// claudeCodeSystemPreamble 是 OAuth 凭据要求的首个 system 块文本。
	claudeCodeSystemPreamble = "You are Claude Code, Anthropic's official CLI for Claude."
)

// claudeCodeSystemBlockJSON 是注入到 system 数组首位的块(带 ephemeral 缓存标记,
// 与官方客户端一致)。
const claudeCodeSystemBlockJSON = `{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}`

// defaultClaudeModelIDs 是未设白名单时对外暴露的当前 Claude 模型集(别名形式,
// Anthropic 侧会解析到带日期的具体版本)。模型演进时可在此维护,或用账号 Models
// 白名单 / 定价页覆盖。
var defaultClaudeModelIDs = []string{
	"claude-opus-4-5",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
}

// DefaultClaudeModelIDsForAccount 返回该 Claude 账号对外可见的原生模型:优先
// 账号 Models 白名单,否则用当前默认集。历史/误配的非 claude-* 条目必须在
// 目录源头过滤,避免 /v1/models 发布一个调度器随后必然拒绝的模型。
func DefaultClaudeModelIDsForAccount(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	account.Mu().RLock()
	whitelist := append([]string(nil), account.Models...)
	account.Mu().RUnlock()
	if len(whitelist) > 0 {
		visible := make([]string, 0, len(whitelist))
		seen := make(map[string]struct{}, len(whitelist))
		for _, model := range whitelist {
			model = strings.TrimSpace(model)
			key := strings.ToLower(model)
			if !strings.HasPrefix(key, "claude-") {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			visible = append(visible, model)
		}
		return visible
	}
	return append([]string(nil), defaultClaudeModelIDs...)
}

// claudeAccountSupportsModel 判断 Claude Code OAuth 账号能否服务指定模型。
// 若账号设置了显式 Models 白名单,以白名单为准;否则默认放行 claude-* 模型。
func claudeAccountSupportsModel(account *auth.Account, model string) bool {
	if account == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(model), "claude-") {
		return false
	}
	account.Mu().RLock()
	whitelist := append([]string(nil), account.Models...)
	account.Mu().RUnlock()
	if len(whitelist) > 0 {
		for _, m := range whitelist {
			if strings.EqualFold(strings.TrimSpace(m), model) {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

// markClaudeNativeRoute 给 Claude 上游响应打上原生路由标记,复用 handler 里既有的
// 原生 Anthropic Messages SSE 透传路径(forwardGrokNativeResponseTo),无需新写流式
// 处理。标记头名沿用现有常量,语义为"上游已是原生目标协议,直接转发不再翻译"。
func markClaudeNativeRoute(resp *http.Response) {
	if resp != nil && resp.Header != nil {
		resp.Header.Set(grokNativeRouteHeader, "1")
	}
}

// ExecuteClaudeMessagesRequest 把入站 Anthropic Messages 请求透传给 Claude Code
// OAuth 账号对应的上游,返回原始上游响应。
func ExecuteClaudeMessagesRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header, fingerprintMode string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// A retry can reuse the request context. Clear any previous attempt's
	// observation before building this attempt so a transport failure cannot
	// make UsageLog attribute the old upstream User-Agent to the new request.
	resetUpstreamUserAgentAudit(ctx)
	if account == nil {
		return nil, ErrNoAvailableAccount()
	}

	account.Mu().RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	proxyURL := account.ProxyURL
	// 该账号绑定的稳定指纹(导入时生成,存于 credentials.custom_headers)。
	fingerprint := cloneStringMap(account.CustomHeaders)
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}

	// 安全净化:去零宽/控制字符 + NFC 归一。不改变可见文字与语义,只让请求更"正常"。
	body := sanitizeClaudeRequestText(requestBody)
	body = injectClaudeCodeSystemPrompt(body)
	stream := gjson.GetBytes(body, "stream").Bool()

	client := getPooledClient(account, proxyURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeMessagesEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建 Claude 请求失败", err)
	}
	applyClaudeMessagesHeaders(req, accessToken, headers, stream, fingerprint, fingerprintMode)

	resp, err := client.Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Anthropic Messages API 失败", err)
	}
	return resp, nil
}

// applyClaudeMessagesHeaders 设置透传请求头。
//
// 指纹一致性策略(由 fingerprintMode 决定,来自账号级覆盖 > 全局默认):
//   - preserve(默认):入站真实 Claude Code 客户端的身份头优先保留,缺失才用账号
//     绑定指纹补齐——它本身就是一致的,伪造反而破坏一致性。
//   - force:无条件用账号绑定指纹覆盖入站身份头,保证该账号对 Anthropic 始终呈现
//     同一套 Claude Code 身份(强制替换,防跨客户端指纹漂移)。
//
// fingerprint 为账号绑定指纹头(规范化头名→值),来自 credentials.custom_headers。
func applyClaudeMessagesHeaders(req *http.Request, accessToken string, incoming http.Header, stream bool, fingerprint map[string]string, fingerprintMode string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	// anthropic-version:优先保留入站真实客户端的值。
	if v := strings.TrimSpace(incoming.Get("anthropic-version")); v != "" {
		req.Header.Set("anthropic-version", v)
	} else {
		req.Header.Set("anthropic-version", claudeAnthropicVersion)
	}
	req.Header.Set("anthropic-beta", mergeAnthropicBeta(incoming))
	// OAuth 凭据不带 x-api-key;若入站客户端塞了，务必剔除避免冲突。
	req.Header.Del("x-api-key")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	// 指纹 map 键大小写不定(来自 custom_headers),统一小写后按小写头名查。
	fpLower := make(map[string]string, len(fingerprint))
	for k, v := range fingerprint {
		fpLower[strings.ToLower(strings.TrimSpace(k))] = v
	}
	// force 模式:账号指纹优先,无条件覆盖入站身份头(有指纹才覆盖,避免抹成空)。
	// preserve 模式:入站有则保留,无则用账号指纹补齐。
	force := auth.NormalizeClaudeFingerprintMode(fingerprintMode) == auth.ClaudeFingerprintModeForce
	for _, name := range auth.ClaudeIdentityHeaderNames {
		fpVal := strings.TrimSpace(fpLower[name])
		if force {
			// Legacy accounts may contain only a partial fingerprint. In force
			// mode every identity header must still be deterministic; otherwise
			// the missing field would inherit a different downstream client and
			// silently defeat the stable-account contract.
			if fpVal == "" {
				fpVal = defaultClaudeIdentityHeader(name)
			}
			if fpVal != "" {
				req.Header.Set(name, fpVal)
			}
			continue
		}
		if v := strings.TrimSpace(incoming.Get(name)); v != "" {
			req.Header.Set(name, v)
			continue
		}
		if fpVal != "" {
			req.Header.Set(name, fpVal)
		}
	}
	// 保底:连指纹都没有(老账号未生成指纹)时,给一个稳定的默认 UA,避免空 UA 破绽。
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
	}
	// Keep Claude on the same request-scoped User-Agent audit path as Codex,
	// Grok, and WebSocket transports. Record only the final sanitized header
	// after preserve/force resolution so the Usage page can show whether the
	// upstream identity was actually rewritten.
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

// defaultClaudeIdentityHeader is a deterministic compatibility fallback for
// legacy accounts whose persisted fingerprint predates one of the current
// Claude Code identity headers. It is deliberately a fixed, provider-shaped
// value rather than a per-request random value, so force mode cannot drift.
func defaultClaudeIdentityHeader(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "user-agent":
		return "claude-cli/2.1.220 (external, cli)"
	case "x-app":
		return "cli"
	case "x-stainless-lang":
		return "js"
	case "x-stainless-package-version":
		return "0.68.0"
	case "x-stainless-os":
		return "Linux"
	case "x-stainless-arch":
		return "x64"
	case "x-stainless-runtime":
		return "node"
	case "x-stainless-runtime-version":
		return "v22.11.0"
	default:
		return ""
	}
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// claudeInvisibleRunes 是应从请求文字中剔除的不可见/格式字符:零宽、词连接符、
// BOM、以及会误导审核/看起来像规避手段的双向控制符。剔除它们让请求更"正常"、
// 反而降低被标记概率,且不改变可见文字与语义。
func claudeInvisibleRune(r rune) bool {
	switch r {
	case 0x200B, 0x200C, 0x200D, // zero-width space / non-joiner / joiner
		0x2060, 0xFEFF, // word joiner / BOM (zero-width no-break space)
		0x180E,                                 // mongolian vowel separator
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // bidi embedding / override / pop
		0x2066, 0x2067, 0x2068, 0x2069: // bidi isolates
		return true
	}
	return false
}

// sanitizeClaudeRequestText 对请求体做安全净化:Unicode NFC 归一 + 剔除不可见/双向
// 控制字符。JSON 的结构字符与键均为 ASCII,不受影响;仅规范化字符串值内的文字。
// 净化后若不再是合法 JSON(理论上不会),回退原始体。
func sanitizeClaudeRequestText(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	normalized := norm.NFC.String(string(body))
	var b strings.Builder
	b.Grow(len(normalized))
	changed := len(normalized) != len(body)
	for _, r := range normalized {
		if claudeInvisibleRune(r) {
			changed = true
			continue
		}
		b.WriteRune(r)
	}
	if !changed {
		return body
	}
	out := []byte(b.String())
	if !gjson.ValidBytes(out) {
		return body
	}
	return out
}

// mergeAnthropicBeta 把入站声明的 anthropic-beta 与 OAuth 必需的 oauth-2025-04-20
// 合并去重,保证 OAuth 头始终在列。
func mergeAnthropicBeta(incoming http.Header) string {
	seen := map[string]struct{}{}
	ordered := make([]string, 0, 4)
	add := func(raw string) {
		for _, part := range strings.Split(raw, ",") {
			v := strings.TrimSpace(part)
			if v == "" {
				continue
			}
			key := strings.ToLower(v)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			ordered = append(ordered, v)
		}
	}
	if incoming != nil {
		add(strings.Join(incoming.Values("anthropic-beta"), ","))
	}
	add(auth.ClaudeOAuthBeta)
	return strings.Join(ordered, ",")
}

// injectClaudeCodeSystemPrompt 保证请求的 system 数组首块是 Claude Code 声明块。
// 兼容三种入站形态:无 system / system 为字符串 / system 为块数组;若首块已是该声明
// 则原样返回,避免重复注入。
func injectClaudeCodeSystemPrompt(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	system := gjson.GetBytes(body, "system")

	switch {
	case !system.Exists() || system.Type == gjson.Null:
		out, err := sjson.SetRawBytes(body, "system", []byte("["+claudeCodeSystemBlockJSON+"]"))
		if err != nil {
			return body
		}
		return out

	case system.Type == gjson.String:
		// 字符串 system → [声明块, {原文本块}]
		orig := system.String()
		if strings.HasPrefix(strings.TrimSpace(orig), claudeCodeSystemPreamble) {
			return body // 已以声明开头,转成数组即可但无需重复
		}
		textBlock, err := sjson.SetBytes([]byte(`{"type":"text"}`), "text", orig)
		if err != nil {
			return body
		}
		raw := "[" + claudeCodeSystemBlockJSON + "," + string(textBlock) + "]"
		out, err := sjson.SetRawBytes(body, "system", []byte(raw))
		if err != nil {
			return body
		}
		return out

	case system.IsArray():
		arr := system.Array()
		if len(arr) > 0 && strings.HasPrefix(strings.TrimSpace(arr[0].Get("text").String()), claudeCodeSystemPreamble) {
			return body // 首块已是声明,不重复注入
		}
		raw := system.Raw
		inner := strings.TrimSpace(raw)
		inner = strings.TrimPrefix(inner, "[")
		inner = strings.TrimSuffix(inner, "]")
		var newArr string
		if strings.TrimSpace(inner) == "" {
			newArr = "[" + claudeCodeSystemBlockJSON + "]"
		} else {
			newArr = "[" + claudeCodeSystemBlockJSON + "," + inner + "]"
		}
		out, err := sjson.SetRawBytes(body, "system", []byte(newArr))
		if err != nil {
			return body
		}
		return out
	}
	return body
}

// ── Claude 统一限流头 → 账号用量快照 ─────────────────────────────────────────
//
// Anthropic 对 Claude Code OAuth 账号的每个响应都带统一限流头(实测 2026-08):
//   anthropic-ratelimit-unified-5h-utilization: 0.01   ← 5h 滚动窗口利用率
//   anthropic-ratelimit-unified-5h-reset:       1787943000 (unix 秒)
//   anthropic-ratelimit-unified-7d-utilization: 0.0    ← 周窗口利用率
//   anthropic-ratelimit-unified-7d-reset:       1788253200
//   anthropic-ratelimit-unified-status:         allowed | rejected
// 该族头为 0-1 小数约定(同响应的 fallback-percentage: 0.5 即 50%)。

// claudeRatelimitHeaderPct 解析 utilization 头为百分数(0-100)。
// 保守起见 >1.5 的值视作上游已改用百分数,不再 ×100,避免进度条爆表。
func claudeRatelimitHeaderPct(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	if f <= 1.5 {
		f *= 100
	}
	if f > 100 {
		f = 100
	}
	return f, true
}

// claudeRatelimitHeaderTime 解析 unix 秒时间戳头(如 *-reset)。
func claudeRatelimitHeaderTime(v string) time.Time {
	v = strings.TrimSpace(v)
	sec, err := strconv.ParseInt(v, 10, 64)
	if err == nil && sec > 0 {
		// Some compatible gateways serialize epoch milliseconds instead of the
		// Anthropic epoch-seconds contract. Normalize that form and reject
		// implausible values so a malformed header cannot create a multi-century
		// account cooldown.
		if sec > 100_000_000_000 {
			sec /= 1000
		}
		if sec >= 946684800 && sec <= 4102444800 { // 2000-01-01 .. 2100-01-01
			return time.Unix(sec, 0)
		}
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if parsed, parseErr := time.Parse(layout, v); parseErr == nil {
			return parsed
		}
	}
	return time.Time{}
}

// SyncClaudeUsageState 解析 Claude 响应的统一限流头,把 5h/7d 窗口利用率与重置
// 时刻写入与 Codex 同源的账号快照字段并持久化——管理页用量进度条/重置倒计时
// 直接生效。429 或 unified-status=rejected 时按上游给的重置时刻精确冷却。
// 持久化调用与 SyncCodexUsageState 同构:persist 在 ApplyUsageObservation 闭包内,
// MarkResponsesPremium5hRateLimited 自带观察序,必须留在闭包外(usageSyncMu 不可重入)。
func SyncClaudeUsageState(store *auth.Store, account *auth.Account, resp *http.Response) {
	if account == nil || resp == nil {
		return
	}
	h := resp.Header
	if h == nil {
		h = make(http.Header)
	}
	pct5h, ok5h := claudeRatelimitHeaderPct(h.Get("anthropic-ratelimit-unified-5h-utilization"))
	reset5h := claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-5h-reset"))
	pct7d, ok7d := claudeRatelimitHeaderPct(h.Get("anthropic-ratelimit-unified-7d-utilization"))
	reset7d := claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-7d-reset"))
	observedAt := time.Now()
	if !ok5h && !ok7d {
		// A valid native response without quota metadata is still evidence that
		// the token was observed. Record freshness without inventing a quota
		// percentage, otherwise the scheduler would repeat a paid probe forever.
		account.MarkClaudeUsageObservation(observedAt)
	}

	if ok5h || ok7d {
		account.ApplyUsageObservation(observedAt, func() {
			if ok5h {
				account.SetUsageSnapshot5hAt(pct5h, reset5h, observedAt)
			}
			if ok7d && !reset7d.IsZero() {
				account.SetReset7dAt(reset7d)
			}
			if store == nil {
				return
			}
			if ok7d {
				store.PersistUsageSnapshot(account, pct7d)
			} else if ok5h {
				store.PersistUsageSnapshot5hOnly(account)
			}
		})
		// A 7d-only unified response is still authoritative for the long
		// window, and therefore also authoritative evidence that a previously
		// cached 5h window is absent. Use the same observation timestamp so a
		// newer concurrent response wins and cannot be erased by this cleanup.
		if ok7d && !ok5h && store != nil {
			if _, hasStale5h := account.GetUsagePercent5h(); hasStale5h {
				store.ClearAbsentUsageSnapshot5hAt(account, observedAt)
			}
		}
	}

	// 上游拒绝(429 / unified-status=rejected)时,必须**按真实耗尽的窗口精确归因**,
	// 否则会把通用/边缘/周窗口的限流一律误标成「5h 窗口 100% 耗尽」并长时间冷却。
	// 注意不匹配 overage-status(那是溢出计费开关,200 响应上也会是 rejected)。
	rejected := resp.StatusCode == http.StatusTooManyRequests ||
		strings.EqualFold(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-status")), "rejected")
	if rejected && store != nil {
		claim := strings.ToLower(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-representative-claim")))
		fiveHourExhausted := (ok5h && pct5h >= 100) || claim == "five_hour" || claim == "five-hour" || claim == "5h"
		sevenDayExhausted := (ok7d && pct7d >= 100) || claim == "seven_day" || claim == "seven-day" || claim == "7d"
		switch {
		case sevenDayExhausted:
			// 周窗口耗尽:记到 7d 窗口(冷却到 7d 重置),不动 5h。上面已按 7d-utilization
			// 持久化;若上游只给了 representative-claim 而无 utilization,则补写 7d=100。
			if !(ok7d && pct7d >= 100) {
				r7 := reset7d
				if r7.IsZero() {
					r7 = claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-reset"))
				}
				account.ApplyUsageObservation(time.Now(), func() {
					account.SetUsageSnapshot(100, time.Now())
					if !r7.IsZero() {
						account.SetReset7dAt(r7)
					}
					store.PersistUsageSnapshot(account, 100)
				})
			}
			store.MarkUsage7dRateLimited(account)
		case fiveHourExhausted:
			// 5h 窗口确实耗尽:标 5h 限流,冷却到 5h 重置。
			resetAt := reset5h
			if resetAt.IsZero() {
				resetAt = claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-reset"))
			}
			store.MarkResponsesPremium5hRateLimited(account, resetAt)
		default:
			// 无任何窗口耗尽信号(通用/边缘/IP 限流,如 rate_limit_error,常无 unified 头)→
			// 只做短退避,绝不标 5h=100%。优先用 Retry-After,否则给保守默认。
			store.MarkCooldown(account, claudeGenericRateLimitBackoff(h), "rate_limited")
		}
	}
}

// claudeGenericRateLimitBackoff 返回通用限流(非窗口耗尽)的短冷却时长:
// 优先取 Retry-After(秒或 HTTP-date),否则默认 1 分钟;上限 15 分钟避免误封过久。
func claudeGenericRateLimitBackoff(h http.Header) time.Duration {
	const def = time.Minute
	const max = 15 * time.Minute
	ra := strings.TrimSpace(h.Get("Retry-After"))
	if ra == "" {
		return def
	}
	if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > max {
			return max
		}
		return d
	}
	if t, err := http.ParseTime(ra); err == nil {
		if d := time.Until(t); d > 0 {
			if d > max {
				return max
			}
			return d
		}
	}
	return def
}
