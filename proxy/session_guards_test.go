package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func newSessionGuardTestHandler(t *testing.T, accounts ...*auth.Account) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	for _, account := range accounts {
		store.AddAccount(account)
	}
	return &Handler{store: store}
}

// disableTurnStateVault 关掉 turn-state 托管（默认开）。托管开启时分类只看替身归属、
// 真实 token 不出网关，溯源表/绑定回退那套 legacy 语义测不到，所以测这套的用例必须
// 显式关掉它。
func disableTurnStateVault(t *testing.T) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexTurnStateVaultEnabled = false; return s })
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

// setStrictTurnState 固定 legacy/strict 两态的分类语义：托管开启时 unknown 一律剥离，
// strict 开关就看不出差别，所以一并关掉托管。
func setStrictTurnState(t *testing.T, strict bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexTurnStateStrict = strict
		s.CodexTurnStateVaultEnabled = false
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
}

func TestApplyCodexTurnStateEchoPolicyClassifiesByExactOrigin(t *testing.T) {
	resetSessionGuardStatsForTest()
	disableTurnStateVault(t)
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	h := newSessionGuardTestHandler(t, minter, other)
	key := "guard-origin::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	noteCodexTurnStateProvenance(key, minter)

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoSame || stripped || headers.Get(codexTurnStateHeader) != "blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "blob" {
		t.Fatalf("same-account echo altered: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}

	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob","thread_id":"t"}}`))
	if class != turnStateEchoCross || !stripped || headers.Get(codexTurnStateHeader) != "" {
		t.Fatalf("cross-account header not stripped: class=%s stripped=%v header=%q", class, stripped, headers.Get(codexTurnStateHeader))
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() || gjson.GetBytes(body, "client_metadata.thread_id").String() != "t" {
		t.Fatalf("cross-account body token must be removed and siblings kept: %s", body)
	}
	totals, accounts := sessionGuardTurnStateSnapshot()
	if totals.Same != 1 || totals.Cross != 1 || totals.Stripped != 1 || len(accounts) != 2 {
		t.Fatalf("counters = %+v accounts=%d", totals, len(accounts))
	}
}

func TestApplyCodexTurnStateEchoPolicyFallsBackToBindingThenUnknown(t *testing.T) {
	resetSessionGuardStatsForTest()
	disableTurnStateVault(t)
	bound := &auth.Account{DBID: 301, AccessToken: "tok"}
	other := &auth.Account{DBID: 302, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, bound, other)
	key := "guard-binding::api-key:9"
	h.store.BindSessionAffinity(key, bound, "")

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, _ := h.applyCodexTurnStateEchoPolicy(key, bound, headers, []byte(`{}`)); class != turnStateEchoSame {
		t.Fatalf("binding-backed same classification = %s", class)
	}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, stripped := h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{}`)); class != turnStateEchoCross || !stripped {
		t.Fatalf("binding-backed cross classification = %s stripped=%v", class, stripped)
	}

	setStrictTurnState(t, false)
	unknownKey := "guard-unknown::api-key:9"
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(unknownKey, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoUnknown || stripped || headers.Get(codexTurnStateHeader) != "blob" || !gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("legacy mode must pass unknown echoes through: class=%s stripped=%v", class, stripped)
	}

	setStrictTurnState(t, true)
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(unknownKey, other, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob"}}`))
	if class != turnStateEchoUnknown || !stripped || headers.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("strict mode must strip unknown echoes: class=%s stripped=%v header=%q body=%s", class, stripped, headers.Get(codexTurnStateHeader), body)
	}

	if _, class, _ := h.applyCodexTurnStateEchoPolicy(unknownKey, other, http.Header{}, []byte(`{}`)); class != turnStateEchoNone {
		t.Fatalf("no token must classify as none: %s", class)
	}
	if _, class, _ := h.applyCodexTurnStateEchoPolicy("", other, headers, []byte(`{}`)); class != turnStateEchoNone {
		t.Fatalf("empty affinity key must not be tracked: %s", class)
	}
}

func TestApplyCodexTurnStateEchoPolicyIgnoresExpiredOrigin(t *testing.T) {
	resetSessionGuardStatsForTest()
	setStrictTurnState(t, false)
	other := &auth.Account{DBID: 402}
	h := newSessionGuardTestHandler(t, other)
	key := "guard-expired::api-key:9"
	codexTurnStateOrigins.Store(key, codexTurnStateOrigin{accountID: 401, expiresAt: time.Now().Add(-time.Minute)})
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	if _, class, _ := h.applyCodexTurnStateEchoPolicy(key, other, headers, []byte(`{}`)); class != turnStateEchoUnknown {
		t.Fatalf("expired origin must fall through to unknown, got %s", class)
	}
}

func TestProjectCodexTurnStateForWebsocket(t *testing.T) {
	setStrictTurnState(t, false)
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	body, out := projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "blob" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("legacy mode must keep the handshake header: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}

	setStrictTurnState(t, true)
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "blob" {
		t.Fatalf("strict mode must move the token into the frame: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
	if headers.Get(codexTurnStateHeader) != "blob" {
		t.Fatal("caller headers must not be mutated")
	}
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"client_metadata":{"x-codex-turn-state":"frame"}}`), headers)
	if out.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "frame" {
		t.Fatalf("existing frame token must win: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
}

// 网关自造的替身一旦没能换回真实 token，就绝不能出网关：`c2a-ts-v1.<32 hex>` 是一枚
// 唯一、稳定、可直接归因到 codex2api 的标记，送到上游等于把托管要消除的那类信号亲手
// 交进对方的风控管线。触发路径就是文档推荐的灰度动作——托管默认开着，运维为了对比把它
// 关掉，在手的客户端下一轮照样回带替身，而溯源表仍把它判成 same、原样放行。
func TestApplyCodexTurnStateEchoPolicyStripsUnresolvedSubstitute(t *testing.T) {
	setVault(t, true)
	resetSessionGuardStatsForTest()
	minter := &auth.Account{DBID: 101, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, minter)
	key := "vault-dead-substitute::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	sub := issueCodexTurnStateSubstitute(key, minter, "real-blob")
	if sub == "" {
		t.Fatal("fixture: the vault must mint a substitute")
	}

	// 回归：本会话当前的替身仍然要换回真实值，剥离规则不能误伤活着的替身。
	live := http.Header{}
	live.Set(codexTurnStateHeader, sub)
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(key, minter, live, []byte(`{"client_metadata":{"x-codex-turn-state":"`+sub+`"}}`))
	if class != turnStateEchoSame || stripped || live.Get(codexTurnStateHeader) != "real-blob" ||
		gjson.GetBytes(body, codexTurnStateBodyPath).String() != "real-blob" {
		t.Fatalf("a live substitute must still be restored: class=%s stripped=%v header=%q body=%s", class, stripped, live.Get(codexTurnStateHeader), body)
	}

	// 托管关闭 + strict 关闭 + 官方账号 + 溯源命中同一账号：修复前分类是 same，原样出站。
	setVault(t, false)
	resetSessionGuardStatsForTest()
	noteCodexTurnStateProvenance(key, minter)
	if got := h.classifyCodexTurnStateEcho(key, minter); got != turnStateEchoSame {
		t.Fatalf("fixture: provenance must classify as same (the pre-fix passthrough), got %s", got)
	}

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, sub)
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(key, minter, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"`+sub+`","thread_id":"t"}}`))
	if class != turnStateEchoUnknown || !stripped {
		t.Fatalf("a dead substitute must be stripped with the vault off: class=%s stripped=%v", class, stripped)
	}
	if got := headers.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("substitute forwarded upstream in the header: %q", got)
	}
	if gjson.GetBytes(body, codexTurnStateBodyPath).Exists() {
		t.Fatalf("substitute forwarded upstream in the body: %s", body)
	}
	if gjson.GetBytes(body, "client_metadata.thread_id").String() != "t" {
		t.Fatalf("sibling body fields damaged: %s", body)
	}

	// 只走帧体的载体（下游 WS 的 client_metadata）同样要剥。
	bodyOnly := http.Header{}
	body, class, stripped = h.applyCodexTurnStateEchoPolicy(key, minter, bodyOnly, []byte(`{"client_metadata":{"x-codex-turn-state":"`+sub+`"}}`))
	if class != turnStateEchoUnknown || !stripped || gjson.GetBytes(body, codexTurnStateBodyPath).Exists() {
		t.Fatalf("body-only substitute must be stripped: class=%s stripped=%v body=%s", class, stripped, body)
	}

	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 2 {
		t.Fatalf("stripped substitutes must be counted as foreign: %+v", v)
	}
	if totals, _ := sessionGuardTurnStateSnapshot(); totals.Unknown != 2 || totals.Stripped != 2 {
		t.Fatalf("observation counters = %+v", totals)
	}
}

// relay 账号不托管（替身没人换得回去），但替身照样不能带去中转上游：同一条无条件规则。
// 这也是评审里「strict 关闭时中转会话的死替身透传」那条延后项的根因。
func TestApplyCodexTurnStateEchoPolicyStripsSubstituteForRelayAccount(t *testing.T) {
	setVault(t, true)
	resetSessionGuardStatsForTest()
	relay := newRelayStyleVaultAccount(t, 505)
	h := newSessionGuardTestHandler(t, relay)
	sub := "c2a-ts-v1." + strings.Repeat("ab", 16)

	for _, tc := range []struct {
		name       string
		key        string
		provenance bool
	}{
		{name: "unknown", key: "relay-substitute-unknown::api-key:9"},
		{name: "same", key: "relay-substitute-same::api-key:9", provenance: true},
	} {
		key := tc.key
		t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
		if tc.provenance {
			noteCodexTurnStateProvenance(key, relay)
		}
		headers := http.Header{}
		headers.Set(codexTurnStateHeader, sub)
		body, class, stripped := h.applyCodexTurnStateEchoPolicy(key, relay, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"`+sub+`"}}`))
		if class != turnStateEchoUnknown || !stripped || headers.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, codexTurnStateBodyPath).Exists() {
			t.Fatalf("%s: relay upstream must never receive a substitute: class=%s stripped=%v header=%q body=%s",
				tc.name, class, stripped, headers.Get(codexTurnStateHeader), body)
		}
	}
	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 2 {
		t.Fatalf("relay strips must be counted as foreign: %+v", v)
	}
}

// strict 模式把出站头里的 token 投进 client_metadata，这是替身绕开握手头那道闸的唯一
// 通路：/v1/chat/completions 与 /v1/messages 都不经 applyCodexTurnStateEchoPolicy，
// 投进去就等于把网关的唯一标记送进上游帧体。替身只删不投，真实 token 照旧。
func TestProjectCodexTurnStateForWebsocketNeverProjectsSubstitute(t *testing.T) {
	setStrictTurnState(t, true)
	resetTurnStateVaultForTest()
	substitute := codexTurnStateSubstitutePrefix + strings.Repeat("9a", 16)

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, substitute)
	body, out := projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "" {
		t.Fatalf("substitute must be dropped from the handshake headers, got %q", out.Get(codexTurnStateHeader))
	}
	if gjson.GetBytes(body, codexTurnStateBodyPath).Exists() {
		t.Fatalf("substitute must never be projected into client_metadata: %s", body)
	}
	if gjson.GetBytes(body, "model").String() != "gpt-5.5" {
		t.Fatalf("the frame body must be returned unchanged: %s", body)
	}
	if headers.Get(codexTurnStateHeader) != substitute {
		t.Fatal("caller headers must not be mutated")
	}
	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 1 {
		t.Fatalf("a dropped projection must be counted as foreign: %+v", v)
	}

	// 回归：真实 token 仍按官方 WS v2 契约挪进帧体。
	headers.Set(codexTurnStateHeader, "real-blob")
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != "" || gjson.GetBytes(body, codexTurnStateBodyPath).String() != "real-blob" {
		t.Fatalf("a real token must still be projected: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 1 {
		t.Fatalf("a real token must not be counted as a drop: %+v", v)
	}

	// strict 关闭：投影整体不生效，替身原样留在头里，交给出站白名单/握手那两道闸处理。
	setStrictTurnState(t, false)
	headers.Set(codexTurnStateHeader, substitute)
	body, out = projectCodexTurnStateForWebsocket([]byte(`{"model":"gpt-5.5"}`), headers)
	if out.Get(codexTurnStateHeader) != substitute || gjson.GetBytes(body, codexTurnStateBodyPath).Exists() {
		t.Fatalf("legacy mode must leave both carriers untouched: header=%q body=%s", out.Get(codexTurnStateHeader), body)
	}
	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 1 {
		t.Fatalf("legacy mode must not count a drop here: %+v", v)
	}
}

func TestDropCodexTurnStateSubstituteFromBody(t *testing.T) {
	resetTurnStateVaultForTest()
	substitute := codexTurnStateSubstitutePrefix + strings.Repeat("7b", 16)

	body, dropped := dropCodexTurnStateSubstituteFromBody([]byte(`{"model":"gpt-5.5","client_metadata":{"x-codex-turn-state":"` + substitute + `","thread_id":"t"}}`))
	if !dropped || gjson.GetBytes(body, codexTurnStateBodyPath).Exists() {
		t.Fatalf("substitute must be removed from the body: dropped=%v body=%s", dropped, body)
	}
	if gjson.GetBytes(body, "client_metadata.thread_id").String() != "t" || gjson.GetBytes(body, "model").String() != "gpt-5.5" {
		t.Fatalf("sibling fields damaged: %s", body)
	}
	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 1 {
		t.Fatalf("a dropped body substitute must be counted as foreign: %+v", v)
	}

	realBody := []byte(`{"client_metadata":{"x-codex-turn-state":"real-blob"}}`)
	out, dropped := dropCodexTurnStateSubstituteFromBody(realBody)
	if dropped || gjson.GetBytes(out, codexTurnStateBodyPath).String() != "real-blob" {
		t.Fatalf("a real token must keep round-one passthrough: dropped=%v body=%s", dropped, out)
	}

	for _, untouched := range [][]byte{[]byte(`not json`), []byte(``), []byte(`{"model":"gpt-5.5"}`)} {
		out, dropped := dropCodexTurnStateSubstituteFromBody(untouched)
		if dropped || string(out) != string(untouched) {
			t.Fatalf("input %q must pass through unchanged: dropped=%v out=%s", untouched, dropped, out)
		}
	}
	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 1 {
		t.Fatalf("only the substitute may be counted: %+v", v)
	}
}

// 端到端：/v1/responses 的中转分支在回带策略之前就返回，转发的又是保留了 client_metadata
// 的客户端 body。这里用真实的上游桩把出站 body 抓下来，确认替身没跟着出去。
func TestResponsesRelayBranchNeverForwardsBodySubstitute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime); resetTurnStateVaultForTest() })
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.CodexForceWebsocket = false
		current.CodexTurnStateVaultEnabled = true
		current.CodexTurnStateStrict = false
		return current
	})
	resetTurnStateVaultForTest()

	var upstreamBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBody.Store(string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_relay","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	handler, _ := newPromptConversationLockTestHandler(t)
	t.Cleanup(handler.store.Stop)
	relay := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL,
		APIKey: "relay-body-substitute", Models: []string{"gpt-4.1-direct"}, PlanType: "api",
	}
	if !relay.IsRelayStyle() {
		t.Fatal("test fixture must be a relay-style account for this branch to run")
	}
	handler.store.AddAccount(relay)

	substitute := codexTurnStateSubstitutePrefix + strings.Repeat("5c", 16)
	body := []byte(`{"model":"gpt-4.1-direct","input":"ordinary request","stream":true,"client_metadata":{"x-codex-turn-state":"` + substitute + `","thread_id":"t"}}`)
	c, recorder := signedBoundPromptConversationContextWithRecorder(t, "relay-body-substitute", newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.9",
	}, body, "0123456789abcdef0123456789abcdef")

	handler.Responses(c)

	forwarded, _ := upstreamBody.Load().(string)
	if forwarded == "" {
		t.Fatalf("the relay upstream was never called: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(forwarded, codexTurnStateSubstitutePrefix) {
		t.Fatalf("a gateway substitute reached the relay upstream body: %s", forwarded)
	}
	if gjson.Get(forwarded, codexTurnStateBodyPath).Exists() {
		t.Fatalf("client_metadata.x-codex-turn-state survived: %s", forwarded)
	}
	if gjson.Get(forwarded, "client_metadata.thread_id").String() != "t" {
		t.Fatalf("sibling client_metadata fields must survive: %s", forwarded)
	}
}
