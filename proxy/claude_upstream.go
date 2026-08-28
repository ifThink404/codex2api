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
	"net/http"
	"strings"

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
func ExecuteClaudeMessagesRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
	applyClaudeMessagesHeaders(req, accessToken, headers, stream, fingerprint)

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
// 指纹一致性策略:
//   - 若入站是**真实 Claude Code 客户端**(自带 user-agent / x-stainless-* 身份头),
//     原样保留其身份——它本身就是一致的,伪造反而破坏一致性。
//   - 若入站缺该身份头(如 OpenAI SDK 等非原生客户端),用该账号绑定的稳定指纹补齐,
//     使这个账号对外始终呈现同一套 Claude Code 身份。
//
// fingerprint 为账号绑定指纹头(规范化头名→值),来自 credentials.custom_headers。
func applyClaudeMessagesHeaders(req *http.Request, accessToken string, incoming http.Header, stream bool, fingerprint map[string]string) {
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
	// 身份头:入站有则保留,无则用账号指纹补齐。
	for _, name := range auth.ClaudeIdentityHeaderNames {
		if v := strings.TrimSpace(incoming.Get(name)); v != "" {
			req.Header.Set(name, v)
			continue
		}
		if v := strings.TrimSpace(fpLower[name]); v != "" {
			req.Header.Set(name, v)
		}
	}
	// 保底:连指纹都没有(老账号未生成指纹)时,给一个稳定的默认 UA,避免空 UA 破绽。
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
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
		0x180E,                         // mongolian vowel separator
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
