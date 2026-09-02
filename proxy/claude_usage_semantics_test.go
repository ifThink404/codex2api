package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestGrokNativeUsage_MessagesParsesCacheCreationBreakdown(t *testing.T) {
	payload := []byte(`{"type":"message","usage":{"input_tokens":2048,"cache_read_input_tokens":1800,"cache_creation_input_tokens":248,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100},"output_tokens":503}}`)
	usage := grokNativeUsage(GrokProtocolMessages, payload)
	if usage == nil {
		t.Fatal("usage must be parsed")
	}
	if usage.CachedTokens != 1800 || usage.CacheWriteTokens != 248 || usage.CacheWrite5mTokens != 148 || usage.CacheWrite1hTokens != 100 {
		t.Fatalf("cache fields = read %d / write %d (5m %d, 1h %d)", usage.CachedTokens, usage.CacheWriteTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens)
	}
	// Raw Anthropic semantics are preserved here; the Claude route converts them.
	if usage.InputTokens != 2048 || usage.OutputTokens != 503 {
		t.Fatalf("raw input/output = %d/%d", usage.InputTokens, usage.OutputTokens)
	}
}

func TestGrokNativeUsage_MessagesFallsBackToTotalCacheCreation(t *testing.T) {
	payload := []byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":4081,"output_tokens":4}}`)
	usage := grokNativeUsage(GrokProtocolMessages, payload)
	if usage.CacheWriteTokens != 4081 || usage.CacheWrite5mTokens != 4081 || usage.CacheWrite1hTokens != 0 {
		t.Fatalf("without a breakdown the total counts as 5m: write %d (5m %d, 1h %d)", usage.CacheWriteTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens)
	}
}

func TestMergeGrokNativeUsage_KeepsCacheWriteFields(t *testing.T) {
	first := &UsageInfo{InputTokens: 10, CacheWriteTokens: 4081, CacheWrite1hTokens: 4081}
	second := &UsageInfo{InputTokens: 10, OutputTokens: 4}
	merged := mergeGrokNativeUsage(first, second)
	if merged.CacheWriteTokens != 4081 || merged.CacheWrite1hTokens != 4081 || merged.OutputTokens != 4 {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestApplyAnthropicUsageSemantics_TotalsInputAcrossCacheBuckets(t *testing.T) {
	usage := newUsageInfo(2048, 503, 0, 1800)
	usage.CacheWriteTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens = 248, 148, 100
	applyAnthropicUsageSemantics(usage)
	if usage.InputTokens != 4096 || usage.PromptTokens != 4096 {
		t.Fatalf("input must be uncached+read+write = 4096, got input %d prompt %d", usage.InputTokens, usage.PromptTokens)
	}
	if usage.TotalTokens != 4096+503 || usage.CachedTokens != 1800 || usage.CacheWrite1hTokens != 100 {
		t.Fatalf("total %d cached %d write1h %d", usage.TotalTokens, usage.CachedTokens, usage.CacheWrite1hTokens)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 1800 {
		t.Fatal("cached token details must survive")
	}
	// Idempotent: a second application must not double count.
	applyAnthropicUsageSemantics(usage)
	if usage.InputTokens != 4096 {
		t.Fatalf("second application changed input to %d", usage.InputTokens)
	}
	applyAnthropicUsageSemantics(nil)
}

func TestInjectClaudeCodeSystemPrompt_InheritsClientCacheTTL(t *testing.T) {
	oneHour := []byte(`{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out := injectClaudeCodeSystemPrompt(oneHour)
	if got := gjson.GetBytes(out, "system.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("injected preamble ttl = %q, want 1h so it does not precede the client's 1h block with a 5m block", got)
	}
	fiveMin := []byte(`{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out = injectClaudeCodeSystemPrompt(fiveMin)
	if gjson.GetBytes(out, "system.0.cache_control.ttl").Exists() {
		t.Fatal("client without 1h must keep the default preamble block")
	}
	none := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out = injectClaudeCodeSystemPrompt(none)
	if !gjson.GetBytes(out, "system.0.cache_control").Exists() || gjson.GetBytes(out, "system.0.cache_control.ttl").Exists() {
		t.Fatal("no client cache_control: default 5m preamble block")
	}
	messagesOnly := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	out = injectClaudeCodeSystemPrompt(messagesOnly)
	if got := gjson.GetBytes(out, "system.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("a 1h block in messages must also make the preamble 1h, got %q", got)
	}
}
