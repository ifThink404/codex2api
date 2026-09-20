package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func setVault(t *testing.T, on bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexTurnStateVaultEnabled = on
		s.CodexTurnStateStrict = false
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous); resetTurnStateVaultForTest() })
	resetTurnStateVaultForTest()
}

func TestTurnStateVaultIssueAndResolve(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	key := "vault-1::api-key:9"
	sub := issueCodexTurnStateSubstitute(key, minter, "real-blob")
	if !strings.HasPrefix(sub, "c2a-ts-v1.") || len(sub) != len("c2a-ts-v1.")+32 || sub == "real-blob" {
		t.Fatalf("substitute = %q", sub)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, sub); real != "real-blob" || class != turnStateEchoSame {
		t.Fatalf("same-account resolve = %q %s", real, class)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, other, sub); real != "" || class != turnStateEchoCross {
		t.Fatalf("cross-account resolve = %q %s", real, class)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, "real-blob"); real != "" || class != turnStateEchoUnknown {
		t.Fatalf("a real token echoed back must be treated as foreign: %q %s", real, class)
	}
	second := issueCodexTurnStateSubstitute(key, minter, "real-blob-2")
	if _, class := resolveCodexTurnStateSubstitute(key, minter, sub); class != turnStateEchoUnknown {
		t.Fatal("a previous turn's substitute must not resolve after a new mint")
	}
	if real, _ := resolveCodexTurnStateSubstitute(key, minter, second); real != "real-blob-2" {
		t.Fatal("latest mint must resolve")
	}
	setVault(t, false)
	if sub := issueCodexTurnStateSubstitute(key, minter, "real"); sub != "" {
		t.Fatalf("disabled vault must not issue: %q", sub)
	}
}

// 同一轮的真实 token 经两个载体下发（HTTP 响应头 + response.metadata 事件）时，
// 客户端不论回带哪个载体上的替身都必须能换回真实值。
func TestTurnStateVaultReusesSubstituteAcrossCarriersWithinTurn(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	key := "vault-carriers::api-key:9"
	fromHeader := issueCodexTurnStateSubstitute(key, minter, "real-blob")
	out := (&Handler{}).vaultCodexTurnStateEvent(nil, key, minter, "response.metadata",
		[]byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"real-blob"}}`))
	fromEvent := gjson.GetBytes(out, "headers.x-codex-turn-state").String()
	if fromHeader == "" || fromEvent != fromHeader {
		t.Fatalf("one turn must use one substitute: header=%q event=%q", fromHeader, fromEvent)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, fromHeader); real != "real-blob" || class != turnStateEchoSame {
		t.Fatalf("header substitute must survive the event carrier: %q %s", real, class)
	}
	if v := turnStateVaultCountersSnapshot(); v.Issued != 1 {
		t.Fatalf("carrier reuse must not double-count mints: %+v", v)
	}
	if next := issueCodexTurnStateSubstitute(key, minter, "real-blob-2"); next == fromHeader {
		t.Fatal("a new turn's token must mint a new substitute")
	}
}

func TestTurnStateVaultExpires(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	key := "vault-exp::api-key:9"
	sub := issueCodexTurnStateSubstitute(key, minter, "real")
	turnStateVault.Range(func(k, v any) bool {
		if k == key {
			entry := v.(*turnStateVaultEntry)
			entry.expiresAt = time.Now().Add(-time.Second)
		}
		return true
	})
	if _, class := resolveCodexTurnStateSubstitute(key, minter, sub); class != turnStateEchoUnknown {
		t.Fatalf("expired entry must be unknown, got %s", class)
	}
}

func TestVaultCodexTurnStateEventRewritesMetadataHeaders(t *testing.T) {
	setVault(t, true)
	h := &Handler{}
	minter := &auth.Account{DBID: 101}
	key := "vault-ev::api-key:9"
	for _, eventType := range []string{"response.metadata", "codex.response.metadata"} {
		data := []byte(`{"type":"` + eventType + `","headers":{"X-Codex-Turn-State":"real-blob","openai-model":"gpt-5.5"},"metadata":{"k":"v"}}`)
		out := h.vaultCodexTurnStateEvent(nil, key, minter, eventType, data)
		got := gjson.GetBytes(out, "headers.X-Codex-Turn-State").String()
		if got == "real-blob" || !strings.HasPrefix(got, "c2a-ts-v1.") {
			t.Fatalf("%s: token not substituted: %s", eventType, out)
		}
		if gjson.GetBytes(out, "headers.openai-model").String() != "gpt-5.5" || gjson.GetBytes(out, "metadata.k").String() != "v" {
			t.Fatalf("%s: sibling fields damaged: %s", eventType, out)
		}
		if real, class := resolveCodexTurnStateSubstitute(key, minter, got); real != "real-blob" || class != turnStateEchoSame {
			t.Fatalf("%s: substitute must resolve: %q %s", eventType, real, class)
		}
	}
	untouched := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if out := h.vaultCodexTurnStateEvent(nil, key, minter, "response.output_text.delta", untouched); string(out) != string(untouched) {
		t.Fatal("non-metadata events must pass through unchanged")
	}
	noToken := []byte(`{"type":"response.metadata","headers":{"openai-model":"gpt-5.5"}}`)
	if out := h.vaultCodexTurnStateEvent(nil, key, minter, "response.metadata", noToken); string(out) != string(noToken) {
		t.Fatal("metadata without a token must pass through unchanged")
	}
}

func TestApplyCodexTurnStateEchoPolicyRestoresSubstitute(t *testing.T) {
	setVault(t, true)
	resetSessionGuardStatsForTest()
	minter := &auth.Account{DBID: 101, AccessToken: "tok"}
	other := &auth.Account{DBID: 202, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, minter, other)
	key := "vault-policy::api-key:9"
	sub := issueCodexTurnStateSubstitute(key, minter, "real-blob")
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, sub)
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(nil, key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"`+sub+`"}}`))
	if class != turnStateEchoSame || stripped || headers.Get(codexTurnStateHeader) != "real-blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "real-blob" {
		t.Fatalf("substitute must be restored on both carriers: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}
	headers.Set(codexTurnStateHeader, sub)
	if _, class, stripped := h.applyCodexTurnStateEchoPolicy(nil, key, other, headers, []byte(`{}`)); class != turnStateEchoCross || !stripped || headers.Get(codexTurnStateHeader) != "" {
		t.Fatalf("cross-account substitute must be stripped: %s %v", class, stripped)
	}
	headers.Set(codexTurnStateHeader, "foreign-real-token")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(nil, key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"foreign-real-token"}}`))
	if class != turnStateEchoUnknown || !stripped || headers.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("foreign token must always be stripped under the vault even with strict off: %s %v", class, stripped)
	}
	_, accounts := sessionGuardTurnStateSnapshot()
	if len(accounts) == 0 {
		t.Fatal("observations must still be counted")
	}
	if v := turnStateVaultCountersSnapshot(); v.Issued != 1 || v.Restored != 1 || v.ForeignStripped != 1 {
		t.Fatalf("vault counters = %+v", v)
	}
}

func newRelayStyleVaultAccount(t *testing.T, dbid int64) *auth.Account {
	t.Helper()
	account := &auth.Account{
		DBID:         dbid,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example.com/v1",
		APIKey:       "relay-key",
	}
	if !account.IsRelayStyle() {
		t.Fatal("test account must be relay-style")
	}
	return account
}

// relay/Grok/Antigravity/Claude 账号的出站不经 applyCodexTurnStateEchoPolicy，
// 给它们发替身等于下一轮把网关自造的值交给上游，续链会断：托管必须放行。
func TestTurnStateVaultSkipsRelayStyleAccounts(t *testing.T) {
	setVault(t, true)
	resetSessionGuardStatsForTest()
	relay := newRelayStyleVaultAccount(t, 505)
	key := "vault-relay-style::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })

	if sub := issueCodexTurnStateSubstitute(key, relay, "real-blob"); sub != "" {
		t.Fatalf("relay account must not mint a substitute: %q", sub)
	}

	c, _ := newTurnStateTestContext(t)
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "real-blob")
	relayCodexTurnStateResponseHeader(c, key, relay, "", upstream)
	if got := c.Writer.Header().Get(codexTurnStateHeader); got != "real-blob" {
		t.Fatalf("relay account header = %q, want the real token passed through", got)
	}

	event := []byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"real-blob"}}`)
	if out := (&Handler{}).vaultCodexTurnStateEvent(nil, key, relay, "response.metadata", event); string(out) != string(event) {
		t.Fatalf("relay account event rewritten: %s", out)
	}

	// 托管开着也按第一轮语义分类：strict 关闭时无溯源的回带原样透传。
	h := newSessionGuardTestHandler(t, relay)
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "real-blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(nil, "vault-relay-unknown::api-key:9", relay, headers,
		[]byte(`{"client_metadata":{"x-codex-turn-state":"real-blob"}}`))
	if class != turnStateEchoUnknown || stripped || headers.Get(codexTurnStateHeader) != "real-blob" ||
		gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "real-blob" {
		t.Fatalf("relay echo must follow round-one policy: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}
}

// 替身铸不出来时必须失败关闭：删头/删事件字段，绝不把真实 token 交给客户端。
func TestTurnStateVaultFailsClosedWhenIssueFails(t *testing.T) {
	setVault(t, true)
	previousGenerator := turnStateSubstituteGenerator
	turnStateSubstituteGenerator = func() string { return "" }
	t.Cleanup(func() { turnStateSubstituteGenerator = previousGenerator })

	minter := &auth.Account{DBID: 101}
	key := "vault-failclosed::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	codexTurnStateOrigins.Delete(key)

	c, _ := newTurnStateTestContext(t)
	c.Writer.Header().Set(codexTurnStateHeader, "stale-from-previous-attempt")
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "real-blob")
	relayCodexTurnStateResponseHeader(c, key, minter, "", upstream)
	if got := c.Writer.Header().Get(codexTurnStateHeader); got != "" {
		t.Fatalf("failed mint must drop the header, got %q", got)
	}
	if _, ok := codexTurnStateOrigins.Load(key); ok {
		t.Fatal("undelivered turn-state must not record provenance")
	}

	out := (&Handler{}).vaultCodexTurnStateEvent(nil, key, minter, "response.metadata",
		[]byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"real-blob","openai-model":"gpt-5.5"}}`))
	if gjson.GetBytes(out, "headers.x-codex-turn-state").Exists() {
		t.Fatalf("failed mint must drop the event field, got %s", out)
	}
	if gjson.GetBytes(out, "headers.openai-model").String() != "gpt-5.5" {
		t.Fatalf("sibling headers damaged: %s", out)
	}
}

// 无会话标识时没有键可以存真实值，托管仍然不许把真实 token 交给客户端：丢头。
func TestTurnStateVaultDropsHeaderWithoutSessionIdentity(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	c, _ := newTurnStateTestContext(t)
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "real-blob")
	relayCodexTurnStateResponseHeader(c, "", minter, "", upstream)
	if got := c.Writer.Header().Get(codexTurnStateHeader); got != "" {
		t.Fatalf("sessionless official request must not receive the real token, got %q", got)
	}
}

func TestCommitResponsesStreamAttemptEmitsSubstitute(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 303}
	key := "vault-commit::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })

	c, recorder := newTurnStateTestContext(t)
	flusher, _ := c.Writer.(http.Flusher)
	attempt := newContinuousRetryStreamAttempt(true, c.Writer, flusher)
	t.Cleanup(func() { _ = attempt.Close() })
	if _, err := attempt.replay.Write([]byte("data: {\"type\":\"response.completed\"}\n\n")); err != nil {
		t.Fatalf("buffer attempt: %v", err)
	}
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "real-blob")

	if err := (&Handler{}).commitResponsesStreamAttempt(c, attempt, key, minter, upstream); err != nil {
		t.Fatalf("commit response attempt: %v", err)
	}
	got := recorder.Result().Header.Get(codexTurnStateHeader)
	if got == "" || got == "real-blob" || !strings.HasPrefix(got, codexTurnStateSubstitutePrefix) {
		t.Fatalf("client must receive a substitute, got %q", got)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, got); real != "real-blob" || class != turnStateEchoSame {
		t.Fatalf("committed substitute must resolve: %q %s", real, class)
	}
}

func TestRelayCodexTurnStateResponseHeaderEmitsSubstitute(t *testing.T) {
	setVault(t, true)
	minter := &auth.Account{DBID: 101}
	key := "vault-relay::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	c, rec := newTurnStateTestContext(t)
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "real-blob")
	relayCodexTurnStateResponseHeader(c, key, minter, "", upstream)
	got := c.Writer.Header().Get(codexTurnStateHeader)
	_ = rec
	if got == "" || got == "real-blob" || !strings.HasPrefix(got, "c2a-ts-v1.") {
		t.Fatalf("client must receive a substitute, got %q", got)
	}
	if real, class := resolveCodexTurnStateSubstitute(key, minter, got); real != "real-blob" || class != turnStateEchoSame {
		t.Fatalf("relayed substitute must resolve: %q %s", real, class)
	}
}
