package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// guardsOffAccount 造一个官方 Codex 账号（非 relay-style），但把
// session_guards_policy 设成 off——这正是本组用例要验的那类账号。
func guardsOffAccount(id int64) *auth.Account {
	return &auth.Account{DBID: id, AccessToken: "tok", SessionGuardsPolicy: auth.SessionGuardsPolicyOff}
}

// (a) 判据本身：官方账号默认在防护内；policy=off 与 relay-style 一样落在防护外。
func TestSessionGuardsActiveForHonoursAccountPolicy(t *testing.T) {
	setVault(t, true)
	official := &auth.Account{DBID: 101, AccessToken: "tok"}
	off := guardsOffAccount(102)
	relay := &auth.Account{DBID: 103, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk"}
	if !relay.IsRelayStyle() {
		t.Fatal("test fixture relay account must satisfy IsRelayStyle()")
	}

	if !sessionGuardsActiveFor(official) || !turnStateVaultAppliesTo(official) {
		t.Fatal("an official account with the default policy must stay inside the session guards")
	}
	if sessionGuardsActiveFor(off) {
		t.Fatal("session_guards_policy=off must leave the account outside the guards")
	}
	if turnStateVaultAppliesTo(off) {
		t.Fatal("session_guards_policy=off must disable the turn-state vault for that account")
	}
	if sessionGuardsActiveFor(relay) || sessionGuardsActiveFor(nil) || sessionGuardsActiveFor(&auth.Account{}) {
		t.Fatal("relay-style, nil and unsaved accounts must all stay outside the guards")
	}
}

// (b) policy=off 的账号：外来的真实 token 原样带出（不分类、不剥离、不计数），
// 但网关自造的替身照旧一律拦下——那是网关的唯一标记，任何开关都不许它出网关。
func TestApplyCodexTurnStateEchoPolicyOffAccountKeepsRealTokenButDropsSubstitute(t *testing.T) {
	resetSessionGuardStatsForTest()
	setVault(t, true)
	minter := &auth.Account{DBID: 201, AccessToken: "tok"}
	off := guardsOffAccount(202)
	h := newSessionGuardTestHandler(t, minter, off)
	key := "guards-off::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	// 溯源指向别的账号：在防护内这一定判成 cross 并被剥离。
	noteCodexTurnStateProvenance(key, minter)

	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	body, class, stripped := h.applyCodexTurnStateEchoPolicy(key, off, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"blob","thread_id":"t"}}`))
	if stripped || headers.Get(codexTurnStateHeader) != "blob" {
		t.Fatalf("guards-off account must keep the inbound header: stripped=%v header=%q", stripped, headers.Get(codexTurnStateHeader))
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "blob" {
		t.Fatalf("guards-off account must keep the inbound body token: %s", body)
	}
	if class == turnStateEchoCross {
		t.Fatal("guards-off account must not be classified at all")
	}
	if totals, accounts := sessionGuardTurnStateSnapshot(); totals != (SessionGuardTurnStateCounters{}) || len(accounts) != 0 {
		t.Fatalf("guards-off account must not move the classification counters: %+v accounts=%d", totals, len(accounts))
	}
	if v := turnStateVaultCountersSnapshot(); v != (SessionGuardVaultCounters{}) {
		t.Fatalf("a real token on a guards-off account must not move the vault counters: %+v", v)
	}

	substitute := codexTurnStateSubstitutePrefix + "deadbeefdeadbeefdeadbeefdeadbeef"
	headers.Set(codexTurnStateHeader, substitute)
	body, _, stripped = h.applyCodexTurnStateEchoPolicy(key, off, headers, []byte(`{"client_metadata":{"x-codex-turn-state":"`+substitute+`","thread_id":"t"}}`))
	if !stripped || headers.Get(codexTurnStateHeader) != "" {
		t.Fatalf("a gateway substitute must never leave the gateway: stripped=%v header=%q", stripped, headers.Get(codexTurnStateHeader))
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("the substitute must be removed from the body too: %s", body)
	}
	if gjson.GetBytes(body, "client_metadata.thread_id").String() != "t" {
		t.Fatalf("sibling fields must survive the drop: %s", body)
	}
	if v := turnStateVaultCountersSnapshot(); v.ForeignStripped != 1 {
		t.Fatalf("the substitute drop must count foreign_stripped exactly once: %+v", v)
	}
	if totals, _ := sessionGuardTurnStateSnapshot(); totals != (SessionGuardTurnStateCounters{}) {
		t.Fatalf("the substitute drop must not move the classification counters: %+v", totals)
	}
}

// (c) policy=off 的账号上的 500 不计连击，会话也就永远不会被自动锁。
func TestSessionAutoLockSkipsAccountsWithGuardsOff(t *testing.T) {
	h, official, _ := newAutoLockTestHandler(t)
	off := guardsOffAccount(777)
	h.store.AddAccount(off)
	key := "guards-off-lock::api-key:9"
	for i := 0; i < 5; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: off.DBID, ErrorMessage: "server_error · An error occurred"})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("guards-off account must never lock the session: %v", err)
	}
	if got := sessionAutoLockSnapshot(h).StreakEntries; got != 0 {
		t.Fatalf("guards-off failures must not even open a streak, got %d", got)
	}
	// 同一把钥匙换成普通官方账号仍然照常计数并落锁——豁免只作用于那一个账号。
	for i := 0; i < 3; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, ErrorMessage: "server_error · An error occurred"})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err == nil {
		t.Fatal("an account with the default policy must still lock after the threshold")
	}
	// 同一个默认策略账号，换成容量降载的 500 就不该落锁：豁免看的是错误性质，
	// 与账号策略无关。
	shedKey := "guards-default-shed::api-key:9"
	for i := 0; i < 5; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, shedKey), &database.UsageLogInput{
			StatusCode: 500, AccountID: official.DBID,
			ErrorMessage: "server_is_overloaded · service_unavailable_error · Our servers are currently overloaded",
		})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, shedKey), shedKey); err != nil {
		t.Fatalf("capacity shed 500s must not lock even on a default-policy account: %v", err)
	}
}

// (d) policy=off 的账号跳过首次会话准入：过期的 v7 会话 ID 也放行，且不采样。
func TestEnforceInitialSessionAdmissionSkipsAccountsWithGuardsOff(t *testing.T) {
	resetSessionGuardStatsForTest()
	resetInitialSessionStatsForTest()
	setInitialAdmission(t, true, 180)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	h := &Handler{store: store}
	off := guardsOffAccount(244)
	official := &auth.Account{DBID: 245, AccessToken: "tok"}
	now := time.Now()
	old := v7At(t, now.Add(-time.Hour))
	body := []byte(`{"model":"gpt-5.5","input":[]}`)
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		return c
	}
	if err := h.enforceInitialSessionAdmission(newCtx(), off, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now); err != nil {
		t.Fatalf("guards-off account must be exempt from initial-session admission: %v", err)
	}
	if _, since := sessionGuardInitialSnapshot(time.Now()); since.Samples != 0 {
		t.Fatalf("the guards-off exemption must not even sample: %+v", since)
	}
	if err := h.enforceInitialSessionAdmission(newCtx(), official, codexHeaders(old), body, resolveRequestSessionIdentity(codexHeaders(old), body), false, now); err == nil {
		t.Fatal("an account with the default policy must still be rejected")
	}
}

// (f) 窗口号记录与其余防护同一判据：policy=off 的账号不再记录窗口号。
func TestPopulateUsageWindowNumberSkipsAccountsWithGuardsOff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set(codexWindowIDHeader, "0199a2b0-1f3c-7c11-8f2e-4b6d9a1c2e30:6")
		return c
	}
	handler := newWindowNumberTestHandler(t, guardsOffAccount(1), officialCodexWindowAccount(2))

	input := &database.UsageLogInput{AccountID: 1}
	handler.populateUsageWindowNumber(newCtx(), input)
	if input.WindowNumber != "" {
		t.Fatalf("guards-off account must not record a window number, got %q", input.WindowNumber)
	}
	input = &database.UsageLogInput{AccountID: 2}
	handler.populateUsageWindowNumber(newCtx(), input)
	if input.WindowNumber != "6" {
		t.Fatalf("an account with the default policy must still record it, got %q", input.WindowNumber)
	}
}
