package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claudeMinThinkingSignatureLen 是可信 thinking 签名的最短长度。真实签名在
// Claude 4+ 上有数百字符；客户端会话文件损坏时会出现空串或 ~24 字符的截断
// 签名（anthropics/claude-code#21726），上游对这类块直接返回
// "Invalid `signature` in `thinking` block"。
const claudeMinThinkingSignatureLen = 48

// claudeThinkingSignatureErrorBodyLimit 限制为识别签名错误而读取的上游错误体大小。
const claudeThinkingSignatureErrorBodyLimit = 64 << 10

// dropUnsignedClaudeThinkingBlocks 在发送前移除 assistant 消息里签名为空或明显
// 截断的 thinking 块。文档允许省略历史 thinking，且丢弃一个必然被上游拒绝的块
// 不会改变可用信息。返回处理后的请求体与移除数量；无改动时原样返回输入。
func dropUnsignedClaudeThinkingBlocks(body []byte) ([]byte, int) {
	return filterClaudeThinkingBlocks(body, func(block gjson.Result) bool {
		if block.Get("type").String() != "thinking" {
			return true
		}
		return len(strings.TrimSpace(block.Get("signature").String())) >= claudeMinThinkingSignatureLen
	})
}

// stripClaudeThinkingBlocks 移除所有 assistant 消息中的 thinking / redacted_thinking
// 块，用于上游报告签名无效后的同账号重试。
func stripClaudeThinkingBlocks(body []byte) ([]byte, int) {
	return filterClaudeThinkingBlocks(body, func(block gjson.Result) bool {
		t := block.Get("type").String()
		return t != "thinking" && t != "redacted_thinking"
	})
}

// filterClaudeThinkingBlocks 对每条 content 为数组的 assistant 消息按 keep 过滤块。
func filterClaudeThinkingBlocks(body []byte, keep func(gjson.Result) bool) ([]byte, int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, 0
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, 0
	}
	out := body
	removed := 0
	for i, msg := range messages.Array() {
		if msg.Get("role").String() != "assistant" {
			continue
		}
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		var kept []string
		dropped := 0
		for _, block := range content.Array() {
			if keep(block) {
				kept = append(kept, block.Raw)
			} else {
				dropped++
			}
		}
		if dropped == 0 {
			continue
		}
		raw := "[" + strings.Join(kept, ",") + "]"
		next, err := sjson.SetRawBytes(out, "messages."+strconv.Itoa(i)+".content", []byte(raw))
		if err != nil {
			return body, 0
		}
		out = next
		removed += dropped
	}
	if removed == 0 {
		return body, 0
	}
	return out, removed
}

// isClaudeThinkingSignatureError 识别 Anthropic 对无效 thinking 签名的 400。
func isClaudeThinkingSignatureError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest || len(body) == 0 {
		return false
	}
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String() + " " + gjson.GetBytes(body, "message").String())
	return strings.Contains(message, "invalid `signature`") && strings.Contains(message, "thinking")
}

// executeClaudeWithThinkingSignatureRetry 执行一次上游调用；若上游以
// "Invalid `signature` in `thinking` block" 拒绝且请求里确实带有 thinking 块，
// 则剥掉全部 thinking 块后在同一账号上重试一次。任何其它结果原样返回，
// 错误体会被重新装回响应供调用方读取。
func executeClaudeWithThinkingSignatureRetry(ctx context.Context, body []byte, exec func(context.Context, []byte) (*http.Response, error)) (*http.Response, error) {
	resp, err := exec(ctx, body)
	if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		return resp, err
	}
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, claudeThinkingSignatureErrorBodyLimit))
	_ = resp.Body.Close()
	if readErr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		return resp, nil
	}
	if !isClaudeThinkingSignatureError(resp.StatusCode, errBody) {
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		return resp, nil
	}
	stripped, n := stripClaudeThinkingBlocks(body)
	if n == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		return resp, nil
	}
	log.Printf("[claude-thinking-signature] 上游拒绝 thinking 签名，剥离 %d 个 thinking 块后同账号重试一次", n)
	retryResp, retryErr := exec(ctx, stripped)
	if retryErr != nil {
		return nil, retryErr
	}
	return retryResp, nil
}

// claudeCacheControlBlockCount 统计请求中已声明 cache_control 的块数
// (system 块、messages 内容块、tools)。Anthropic 最多允许 4 个。
func claudeCacheControlBlockCount(body []byte) int {
	count := 0
	countIn := func(items gjson.Result) {
		if !items.IsArray() {
			return
		}
		for _, item := range items.Array() {
			if item.Get("cache_control").Exists() {
				count++
			}
		}
	}
	countIn(gjson.GetBytes(body, "system"))
	countIn(gjson.GetBytes(body, "tools"))
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		for _, msg := range messages.Array() {
			countIn(msg.Get("content"))
		}
	}
	return count
}
