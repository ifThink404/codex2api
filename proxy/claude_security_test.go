package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestNormalizeClaudeRequestBodyCanonicalizesBeforeReview(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"he` + string(rune(0x200B)) + `llo"}],"service_tier":"priority","inference_geo":"us","speed":"fast","safety_identifier":"user-42"}`)
	out, err := normalizeClaudeRequestBody(body, auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hello" {
		t.Fatalf("canonical text = %q, want hello", got)
	}
	for _, field := range []string{"service_tier", "inference_geo", "speed", "safety_identifier"} {
		if gjson.GetBytes(out, field).Exists() {
			t.Fatalf("default security policy kept %s: %s", field, out)
		}
	}
}

func TestNormalizeClaudeRequestBodyDoesNotInjectOAuthPreamble(t *testing.T) {
	out, err := normalizeClaudeRequestBody([]byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}]}`), auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "system").Exists() {
		t.Fatalf("request canonicalizer should not add native OAuth system metadata: %s", out)
	}
}

func TestNormalizeClaudeRequestBodyAllowsExplicitSensitiveFields(t *testing.T) {
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.AllowServiceTier = true
	cfg.AllowInferenceGeo = true
	cfg.AllowSpeed = true
	cfg.AllowSafetyIdentifier = true
	out, err := normalizeClaudeRequestBody([]byte(`{"model":"claude-sonnet-5","messages":[],"service_tier":"priority","inference_geo":"us","speed":"fast","safety_identifier":"user-42"}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"service_tier", "inference_geo", "speed", "safety_identifier"} {
		if !gjson.GetBytes(out, field).Exists() {
			t.Fatalf("explicitly allowed field %s was removed", field)
		}
	}
}

func TestNormalizeClaudeRequestBodyRejectsResourceLimits(t *testing.T) {
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.MaxOutputTokens = 8
	cfg.MaxToolCount = 1
	cfg.MaxToolSchemaBytes = 32
	tooManyTokens := []byte(`{"model":"claude-sonnet-5","max_tokens":9,"messages":[]}`)
	if _, err := normalizeClaudeRequestBody(tooManyTokens, cfg); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("max_tokens overflow error = %v", err)
	}
	if _, err := normalizeClaudeRequestBody([]byte(`{"model":"claude-sonnet-5","max_tokens":8.5,"messages":[]}`), cfg); err == nil || !strings.Contains(err.Error(), "integer") {
		t.Fatalf("fractional max_tokens error = %v", err)
	}
	tooManyTools := []byte(`{"model":"claude-sonnet-5","messages":[],"tools":[{"name":"one","input_schema":{"type":"object"}},{"name":"two","input_schema":{"type":"object"}}]}`)
	if _, err := normalizeClaudeRequestBody(tooManyTools, cfg); err == nil || !strings.Contains(err.Error(), "tools") {
		t.Fatalf("tool count overflow error = %v", err)
	}
	tooLargeSchema := []byte(`{"model":"claude-sonnet-5","messages":[],"tools":[{"name":"one","input_schema":{"description":"this schema is intentionally longer than thirty-two bytes"}}]}`)
	if _, err := normalizeClaudeRequestBody(tooLargeSchema, cfg); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("tool schema overflow error = %v", err)
	}
}

func TestMergeAnthropicBetaUsesRequiredAndAllowlist(t *testing.T) {
	incoming := http.Header{}
	incoming.Set("anthropic-beta", "unknown-beta, approved-beta, oauth-2025-04-20")
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.AllowedBetaHeaders = []string{"approved-beta"}
	got := mergeAnthropicBetaWithConfig(incoming, cfg)
	if !strings.Contains(got, "oauth-2025-04-20") || !strings.Contains(got, "approved-beta") || strings.Contains(got, "unknown-beta") {
		t.Fatalf("filtered Beta headers = %q", got)
	}
}
