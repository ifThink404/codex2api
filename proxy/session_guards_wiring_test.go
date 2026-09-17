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
	if got := regexp.MustCompile(`noteCodexTurnStateProvenance\(affinityKey, account\)`).FindAll(handler, -1); len(got) < 2 {
		t.Fatalf("handler.go provenance notes = %d, want >= 2", len(got))
	}
	if got := regexp.MustCompile(`noteCodexTurnStateProvenance\(affinityKey, account\)`).FindAll(ws, -1); len(got) != 1 {
		t.Fatalf("responses_ws.go provenance notes = %d, want 1", len(got))
	}
	if !regexp.MustCompile(`projectCodexTurnStateForWebsocket\(requestBody, headers\)`).Match(executor) {
		t.Fatal("executor.go WS branch must project the turn-state into the frame")
	}
}
