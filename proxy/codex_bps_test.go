package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestPrepareCodexBPSBodyAdaptsResponsesInput(t *testing.T) {
	body := []byte(`{"model":"codex-auto-review","input":"hello","instructions":"be concise","tools":[{"type":"function","name":"lookup"}],"reasoning":{"effort":"high"},"stream":true}`)
	projected, diagnostic, err := prepareCodexBPSBody(body, "cache-1", false)
	if err != nil {
		t.Fatalf("prepareCodexBPSBody: %v", err)
	}
	if diagnostic.SentModel != "gpt-5.6-luna" {
		t.Fatalf("sent model = %q", diagnostic.SentModel)
	}
	var got map[string]any
	if err := json.Unmarshal(projected, &got); err != nil {
		t.Fatalf("decode projected body: %v", err)
	}
	if got["model"] != "gpt-5.6-luna" || got["prompt_cache_key"] != "cache-1" {
		t.Fatalf("unexpected envelope: %#v", got)
	}
	items, ok := got["input"].([]any)
	if !ok || len(items) != 4 {
		t.Fatalf("input items = %#v", got["input"])
	}
	if !strings.Contains(strings.Join(diagnostic.AdaptedFields, ","), "tools") {
		t.Fatalf("tools adaptation missing: %#v", diagnostic.AdaptedFields)
	}
}

func TestApplyCodexBPSHeaders(t *testing.T) {
	account := &auth.Account{DBID: 7, AccountID: "workspace-7"}
	headers := make(http.Header)
	applyCodexBPSHeaders(headers, account, "access-token", "cache-7", false)
	for key, want := range map[string]string{
		"Authorization":      "Bearer access-token",
		"Chatgpt-Account-Id": "workspace-7",
		"Origin":             "https://bps.openai.com",
		"Session-Id":         "cache-7",
		"Accept":             "text/event-stream",
	} {
		if got := headers.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if !IsCodexBPSEndpoint(CodexBPSBaseURL+"/responses") || IsCodexBPSEndpoint("https://api.openai.com/v1/responses") {
		t.Fatal("BPS endpoint classification mismatch")
	}
}

func TestCodexBPSAccountSwitch(t *testing.T) {
	account := &auth.Account{AccessToken: "at", CodexBPS: true}
	if !account.CodexBPSEnabled() {
		t.Fatal("enabled BPS account was not recognized")
	}
	account.UpstreamType = auth.UpstreamOpenAIResponses
	account.BaseURL = "https://relay.example"
	account.APIKey = "sk-test"
	if account.CodexBPSEnabled() {
		t.Fatal("relay account must not use BPS OAuth transport")
	}
}
