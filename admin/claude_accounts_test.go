package admin

import "testing"

func TestClaudeOAuthPutTake_OneTimeUse(t *testing.T) {
	claudeOAuthPut("state-a", "verifier-a")
	v, ok := claudeOAuthTake("state-a")
	if !ok || v != "verifier-a" {
		t.Fatalf("首次 take 应成功返回 verifier, got=(%q,%v)", v, ok)
	}
	// 一次性:再次 take 应失败。
	if _, ok := claudeOAuthTake("state-a"); ok {
		t.Fatal("同一 state 不应被 take 两次")
	}
}

func TestClaudeOAuthTake_Missing(t *testing.T) {
	if _, ok := claudeOAuthTake("no-such-state"); ok {
		t.Fatal("不存在的 state 应返回 false")
	}
}
