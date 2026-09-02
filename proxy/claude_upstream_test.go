package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestInjectClaudeCodeSystemPrompt_Absent(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	sys := gjson.GetBytes(out, "system")
	if !sys.IsArray() || sys.Array()[0].Get("text").String() != claudeCodeSystemPreamble {
		t.Fatalf("首块应为 Claude Code 声明, got=%s", sys.Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_String(t *testing.T) {
	body := []byte(`{"system":"be helpful","messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	sys := gjson.GetBytes(out, "system")
	arr := sys.Array()
	if len(arr) != 2 {
		t.Fatalf("应为 [声明块, 原文本块], got len=%d raw=%s", len(arr), sys.Raw)
	}
	if arr[0].Get("text").String() != claudeCodeSystemPreamble {
		t.Errorf("首块应为声明, got=%s", arr[0].Raw)
	}
	if arr[1].Get("text").String() != "be helpful" {
		t.Errorf("次块应保留原文本, got=%s", arr[1].Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_Array(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"custom"}],"messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	arr := gjson.GetBytes(out, "system").Array()
	if len(arr) != 2 || arr[0].Get("text").String() != claudeCodeSystemPreamble || arr[1].Get("text").String() != "custom" {
		t.Fatalf("应在数组首位插入声明块, got=%s", gjson.GetBytes(out, "system").Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_AlreadyPresent(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"x"}],"messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	arr := gjson.GetBytes(out, "system").Array()
	if len(arr) != 2 {
		t.Fatalf("首块已是声明,不应重复注入, got len=%d", len(arr))
	}
}

func TestInjectClaudeCodeSystemPrompt_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	out := injectClaudeCodeSystemPrompt(body)
	if gjson.GetBytes(out, "model").String() != "claude-x" || gjson.GetBytes(out, "max_tokens").Int() != 100 {
		t.Fatal("注入不应破坏其它字段")
	}
	if gjson.GetBytes(out, "messages.0.content").String() != "hi" {
		t.Fatal("messages 应保留")
	}
}

func TestMergeAnthropicBeta(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "foo-1, bar-2")
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.AllowedBetaHeaders = []string{"foo-1", "bar-2"}
	got := mergeAnthropicBetaWithConfig(h, cfg)
	// 必须包含 oauth beta 且入站的两个 beta 都在
	for _, want := range []string{"oauth-2025-04-20", "foo-1", "bar-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("合并结果缺少 %s: %s", want, got)
		}
	}
}

func TestMergeAnthropicBeta_Dedup(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "oauth-2025-04-20")
	got := mergeAnthropicBetaWithConfig(h, auth.DefaultClaudeSecurityConfig())
	if strings.Count(got, "oauth-2025-04-20") != 1 {
		t.Fatalf("oauth beta 应去重, got=%s", got)
	}
}

func TestMergeAnthropicBeta_Empty(t *testing.T) {
	got := mergeAnthropicBeta(nil)
	if got != "oauth-2025-04-20" {
		t.Fatalf("空入站时应仅有 oauth beta, got=%s", got)
	}
}

func TestSanitizeClaudeRequestText_StripsZeroWidth(t *testing.T) {
	// 把字面 UTF-8 零宽空格(U+200B)与 BOM(U+FEFF)直接拼进 JSON 字符串值,
	// 模拟真实客户端发送的未转义不可见字符(runtime 构造,源码不含 BOM)。
	content := "he" + string(rune(0x200B)) + "llo" + string(rune(0xFEFF)) + " world"
	body := []byte(`{"messages":[{"role":"user","content":"` + content + `"}]}`)
	out := sanitizeClaudeRequestText(body)
	got := gjson.GetBytes(out, "messages.0.content").String()
	if got != "hello world" {
		t.Fatalf("零宽/BOM 未被清理: %q", got)
	}
	if !gjson.ValidBytes(out) {
		t.Fatal("净化后应仍是合法 JSON")
	}
}

func TestSanitizeClaudeRequestText_KeepsNormal(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[{"role":"user","content":"正常中文与English混排"}]}`)
	out := sanitizeClaudeRequestText(body)
	if gjson.GetBytes(out, "messages.0.content").String() != "正常中文与English混排" {
		t.Fatal("正常文字不应被改动")
	}
}

func TestApplyClaudeMessagesHeaders_PreservesIncoming(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("user-agent", "claude-cli/9.9.9 (external, cli)")
	incoming.Set("x-stainless-os", "MacOS")
	fp := map[string]string{"User-Agent": "claude-cli/1.0.0 (external, cli)", "X-Stainless-OS": "Linux"}
	applyClaudeMessagesHeaders(req, "tok", incoming, false, fp, "")
	// 入站真实客户端头应优先保留,不被指纹覆盖。
	if req.Header.Get("User-Agent") != "claude-cli/9.9.9 (external, cli)" {
		t.Fatalf("应保留入站 UA, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Stainless-Os") != "MacOS" {
		t.Fatalf("应保留入站 x-stainless-os, got %s", req.Header.Get("X-Stainless-Os"))
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Fatal("Authorization 应被设置")
	}
}

func TestApplyClaudeMessagesHeaders_UsesFingerprintWhenAbsent(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	fp := map[string]string{
		"User-Agent":     "claude-cli/2.1.220 (external, cli)",
		"X-App":          "cli",
		"X-Stainless-OS": "Linux",
	}
	applyClaudeMessagesHeaders(req, "tok", http.Header{}, false, fp, "")
	if req.Header.Get("User-Agent") != "claude-cli/2.1.220 (external, cli)" {
		t.Fatalf("缺入站头时应用指纹 UA, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-App") != "cli" {
		t.Fatalf("应用指纹 x-app, got %s", req.Header.Get("X-App"))
	}
	if req.Header.Get("Anthropic-Beta") == "" || !strings.Contains(req.Header.Get("Anthropic-Beta"), "oauth-2025-04-20") {
		t.Fatal("anthropic-beta 应含 oauth")
	}
}

func TestApplyClaudeMessagesHeaders_ForceOverridesIncoming(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("user-agent", "claude-cli/9.9.9 (external, cli)")
	incoming.Set("x-stainless-os", "MacOS")
	fp := map[string]string{"User-Agent": "claude-cli/1.0.0 (external, cli)", "X-Stainless-OS": "Linux"}
	applyClaudeMessagesHeaders(req, "tok", incoming, false, fp, "force")
	// force 模式:账号指纹无条件覆盖入站身份头。
	if req.Header.Get("User-Agent") != "claude-cli/1.0.0 (external, cli)" {
		t.Fatalf("force 应用指纹 UA, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Stainless-Os") != "Linux" {
		t.Fatalf("force 应用指纹 x-stainless-os, got %s", req.Header.Get("X-Stainless-Os"))
	}
}

func TestApplyClaudeMessagesHeadersRecordsFinalUserAgent(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req = req.WithContext(withUserAgentAudit(context.Background()))
	incoming := http.Header{}
	incoming.Set("User-Agent", "curl/8.7.1")
	fingerprint := map[string]string{"User-Agent": "claude-cli/2.1.220 (external, cli)"}

	applyClaudeMessagesHeaders(req, "tok", incoming, false, fingerprint, "force")

	got, ok := upstreamUserAgentAudit(req.Context())
	if !ok {
		t.Fatal("Claude 出站请求应记录最终 User-Agent")
	}
	if got != "claude-cli/2.1.220 (external, cli)" {
		t.Fatalf("审计的 upstream User-Agent = %q, want stable fingerprint", got)
	}
}

func TestExecuteClaudeMessagesRequestClearsStaleUserAgentAudit(t *testing.T) {
	ctx := withUserAgentAudit(context.Background())
	RecordUpstreamUserAgent(ctx, "stale-client/1.0")
	account := &auth.Account{UpstreamType: auth.UpstreamClaude}
	_, _ = ExecuteClaudeMessagesRequest(ctx, account, []byte(`{"model":"claude-haiku-4-5","messages":[]}`), "", nil, "force")

	if _, ok := upstreamUserAgentAudit(ctx); ok {
		t.Fatal("Claude attempt should clear a previous attempt's User-Agent audit before transport")
	}
}

func TestApplyClaudeMessagesHeadersForceCompletesPartialFingerprint(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("User-Agent", "curl/8.7.1")
	incoming.Set("X-Stainless-OS", "MacOS")
	fingerprint := map[string]string{"User-Agent": "claude-cli/2.1.220 (external, cli)"}

	applyClaudeMessagesHeaders(req, "tok", incoming, false, fingerprint, "force")
	if req.Header.Get("User-Agent") != "claude-cli/2.1.220 (external, cli)" {
		t.Fatalf("force UA = %q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Stainless-OS") == "MacOS" || strings.TrimSpace(req.Header.Get("X-Stainless-OS")) == "" {
		t.Fatalf("force must not inherit a partial fingerprint's inbound OS: %q", req.Header.Get("X-Stainless-OS"))
	}
	for _, name := range auth.ClaudeIdentityHeaderNames {
		if strings.TrimSpace(req.Header.Get(name)) == "" {
			t.Fatalf("force fingerprint missing %s", name)
		}
	}
}

func TestApplyClaudeMessagesHeadersRewritesFixedClaudeCLIVersion(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("User-Agent", "claude-cli/2.1.205 (external, cli)")
	applyClaudeMessagesHeadersWithVersion(req, "tok", incoming, false, nil, "preserve", "2.1.251")
	if got := req.Header.Get("User-Agent"); got != "claude-cli/2.1.251 (external, cli)" {
		t.Fatalf("fixed Claude CLI UA = %q", got)
	}
}

func TestAlignClaudeOutboundUserAgent(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("")
	cases := []struct {
		name, outbound, required, wantUA string
		wantDeny                         bool
	}{
		{"no requirement", "claude-cli/2.1.219 (external, cli)", "", "claude-cli/2.1.219 (external, cli)", false},
		{"already satisfied", "claude-cli/2.1.258 (external, cli)", "2.1.251", "claude-cli/2.1.258 (external, cli)", false},
		{"stale fingerprint bumped to effective", "claude-cli/2.1.219 (external, cli)", "2.1.251", "claude-cli/" + auth.BuiltinClaudeCLIVersion + " (external, cli)", false},
		{"non-cli untouched", "Go-http-client/1.1", "2.1.251", "Go-http-client/1.1", false},
		{"effective still too old", "claude-cli/2.1.219 (external, cli)", "9.9.9", "claude-cli/2.1.219 (external, cli)", true},
	}
	for _, tc := range cases {
		gotUA, deny := alignClaudeOutboundUserAgent(tc.outbound, tc.required)
		if gotUA != tc.wantUA || (deny != "") != tc.wantDeny {
			t.Errorf("%s: ua=%q deny=%q", tc.name, gotUA, deny)
		}
	}
}

func TestExecuteClaudeMessagesRequestWithPolicy_DeniesWhenForcedFingerprintTooOld(t *testing.T) {
	ctx := withUserAgentAudit(context.Background())
	account := &auth.Account{DBID: 251, UpstreamType: auth.UpstreamClaude, AccessToken: "tok", CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/9.9.9 (external, cli)")
	policy := auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformAny, VersionPolicy: auth.ClaudeVersionPolicyMinimum, ClientVersion: "9.9.9"}
	_, err := ExecuteClaudeMessagesRequestWithPolicy(ctx, account, []byte(`{"model":"claude-opus-5","messages":[]}`), "", headers, "force", policy)
	var perr *Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusUpgradeRequired || perr.Code != "claude_client_policy" {
		t.Fatalf("expected local 426 claude_client_policy, got %v", err)
	}
	if !strings.Contains(perr.Message, "2.1.219") || !strings.Contains(perr.Message, "9.9.9") {
		t.Fatalf("message should name outbound and required versions: %s", perr.Message)
	}
}

func TestClaudeOutboundRequiredVersion_UsesModelFloorForNonCLIInbound(t *testing.T) {
	// 入站不是 CLI(无 required),但 force 指纹是旧 CLI UA 且模型有下限:出站仍需对齐。
	gotUA, deny := alignClaudeOutboundUserAgent("claude-cli/2.1.219 (external, cli)", claudeOutboundRequiredVersion(auth.ClaudeClientDecision{}, "claude-fable-5-1"))
	if deny != "" || !strings.Contains(gotUA, auth.BuiltinClaudeCLIVersion) {
		t.Fatalf("ua=%q deny=%q", gotUA, deny)
	}
}

func TestIsClaudeClientCompatibilityError(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"Claude Code 2.1.205 does not support this model; version 2.1.251 or newer is required."}}`)
	if !isClaudeClientCompatibilityError(http.StatusBadRequest, body) {
		t.Fatal("version-gated Claude 400 should be classified as client compatibility")
	}
	if isClaudeClientCompatibilityError(http.StatusBadRequest, []byte(`{"error":{"type":"invalid_request_error","message":"invalid max_tokens"}}`)) {
		t.Fatal("ordinary invalid request must not be classified as client compatibility")
	}
}
