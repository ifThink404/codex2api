package proxy

import (
	"os"
	"regexp"
	"testing"
)

// 源码守卫：两条尝试循环和 WS 出站都必须经过策略/投影，成功路径必须记录溯源。
func TestSessionGuardWiringPresent(t *testing.T) {
	handler, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.ReadFile("responses_ws.go")
	if err != nil {
		t.Fatal(err)
	}
	executor, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`\n\s*guardCodexTurnStateEcho\(affinityKey, account, downstreamHeaders\)`).Match(handler) {
		t.Fatal("handler.go still calls the legacy guard directly; use applyCodexTurnStateEchoPolicy")
	}
	if got := regexp.MustCompile(`applyCodexTurnStateEchoPolicy\(affinityKey, account, downstreamHeaders, upstreamBody\)`).FindAll(handler, -1); len(got) != 1 {
		t.Fatalf("handler.go policy call sites = %d, want 1", len(got))
	}
	if got := regexp.MustCompile(`applyCodexTurnStateEchoPolicy\(affinityKey, account, downstreamHeaders, upstreamBody\)`).FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go policy call sites = %d, want 1", len(got))
	}
	if got := regexp.MustCompile(`noteCodexTurnStateProvenance\(affinityKey, account\)`).FindAll(handler, -1); len(got) != 1 {
		t.Fatalf("handler.go provenance notes = %d, want 1 (official Codex success site only)", len(got))
	}
	if got := regexp.MustCompile(`noteCodexTurnStateProvenance\(affinityKey, account\)`).FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go provenance notes = %d, want 1", len(got))
	}
	if !regexp.MustCompile(`projectCodexTurnStateForWebsocket\(requestBody, headers\)`).Match(executor) {
		t.Fatal("executor.go WS branch must project the turn-state into the frame")
	}
	if got := regexp.MustCompile(`h\.enforceInitialSessionAdmission\(c, account, c\.Request\.Header, rawBody, sessionIdentity, turnHasBinding,`).FindAll(handler, -1); len(got) != 1 {
		t.Fatalf("handler.go admission call sites = %d, want 1", len(got))
	}
	if got := regexp.MustCompile(`h\.enforceInitialSessionAdmission\(c, account, c\.Request\.Header, rawBody, sessionIdentity, turnHasBinding,`).FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go admission call sites = %d, want 1", len(got))
	}
	if regexp.MustCompile(`h\.checkInitialSessionAdmission\(`).Match(handler) || regexp.MustCompile(`h\.checkInitialSessionAdmission\(`).Match(ws) {
		t.Fatal("pre-selection admission call must be gone")
	}
	unbindAfterAdmission := regexp.MustCompile(`(?s)h\.enforceInitialSessionAdmission\([^\n]*\n(?:[^\n]*\n){0,5}?[^\n]*h\.store\.UnbindSessionAffinity\(affinityKey, account\.ID\(\)\)`)
	if got := unbindAfterAdmission.FindAll(handler, -1); len(got) != 1 {
		t.Fatalf("handler.go admission-rejection cleanup must unbind the session affinity exactly once, got %d", len(got))
	}
	if got := unbindAfterAdmission.FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go admission-rejection cleanup must unbind the session affinity exactly once, got %d", len(got))
	}
	if !regexp.MustCompile(`api\.SendErrorWithStatus\(c, failure, http\.StatusBadRequest\)`).Match(handler) {
		t.Fatal("handler.go must reject initial-session admission failures with HTTP 400 (SendErrorWithStatus), not the default 500")
	}
	for name, src := range map[string][]byte{"handler.go": handler, "responses_ws.go": ws} {
		if got := regexp.MustCompile(`h\.checkSessionAutoLock\(c, affinityKey\)`).FindAll(src, -1); len(got) != 1 {
			t.Fatalf("%s auto-lock check sites = %d, want 1", name, len(got))
		}
		if got := regexp.MustCompile(`h\.rememberSessionAutoLockKey\(c, affinityKey\)`).FindAll(src, -1); len(got) != 1 {
			t.Fatalf("%s auto-lock key sites = %d, want 1", name, len(got))
		}
	}
	if !regexp.MustCompile(`h\.observeSessionAutoLock\(c, input\)`).Match(handler) {
		t.Fatal("logUsageForRequest must feed the auto-lock streaks")
	}
}
