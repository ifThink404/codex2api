package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const deferredPromptBlockToken = "FORBIDDEN_TOKEN"

// newDeferredPromptBlockHandler 造一个「本地规则命中即 block」的网关：自定义规则
// 只认 FORBIDDEN_TOKEN，会话锁打开，这样「先判后拦」的拦截分支能连带验证锁会话。
func newDeferredPromptBlockHandler(t *testing.T, mode string) (*Handler, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "deferred-prompt-block.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:               2,
		TestConcurrency:              1,
		PromptFilterEnabled:          true,
		PromptFilterMode:             mode,
		PromptFilterThreshold:        50,
		PromptFilterStrictThreshold:  90,
		PromptFilterLogMatches:       true,
		PromptFilterMaxTextLength:    promptfilter.DefaultMaxTextLength,
		PromptFilterCustomPatterns:   `[{"name":"deferred_block_token","pattern":"forbidden_token","weight":100,"strict":true,"category":"test"}]`,
		PromptFilterDisabledPatterns: "[]",
	})
	cfg := store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.ConversationLockEnabled = true
	store.SetPromptFilterConfig(cfg)
	// 只有 API Key 101 绑定 NewAPI；其余用例用 API Key 7，走未签名的降级身份。
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}})
	handler := NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, db
}

// newDeferredPromptBlockContext 给上下文配齐 API Key + Codex 窗口标识，
// 让会话锁能解析出 codex-local 降级身份。windowID 不同 = 锁键不同。
func newDeferredPromptBlockContext(t *testing.T, body []byte, windowID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("X-Codex-Window-Id", windowID)
	c.Set(contextAPIKeyID, int64(7))
	setIngressRequestBodyIfAbsent(c, body)
	return c, recorder
}

// newSignedDeferredPromptBlockContext 造一个 NewAPI 签名已验证的上下文，用来覆盖
// 审计里 signed_response 改写那条分支。
func newSignedDeferredPromptBlockContext(t *testing.T, requestID string, body []byte, fingerprint string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, recorder := signedNewAPIPolicyContextWithSecret(t, requestID, newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body, "gateway-a-secret")
	c.Set(contextAPIKeyID, int64(101))
	addSignedNewAPIPolicyMetaWithSecret(t, c, newAPIPolicyMeta{
		PlatformID: "gateway-a", Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses), SessionFingerprint: fingerprint,
	}, true, "gateway-a-secret")
	setIngressRequestBodyIfAbsent(c, body)
	return c, recorder
}

// newSignedDeferredPromptBlockWSContext 是 WS 版本：升级请求没有请求体，NewAPI 的
// 签名因此覆盖空 body，判定时也按空 body 校验。
func newSignedDeferredPromptBlockWSContext(t *testing.T, requestID string, fingerprint string) *gin.Context {
	t.Helper()
	c, _ := signedNewAPIPolicyContextWithSecret(t, requestID, newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", nil, "gateway-a-secret")
	c.Set(contextAPIKeyID, int64(101))
	addSignedNewAPIPolicyMetaWithSecret(t, c, newAPIPolicyMeta{
		PlatformID: "gateway-a", Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses), SessionFingerprint: fingerprint,
	}, true, "gateway-a-secret")
	return c
}

// resetPromptPolicyCounters 归零计数并在用例结束后再归零一次,避免影响相邻用例。
func resetPromptPolicyCounters(t *testing.T) {
	t.Helper()
	resetPromptPolicyCountersForTest()
	t.Cleanup(resetPromptPolicyCountersForTest)
}

func deferredPromptBlockBody(text string) []byte {
	return []byte(`{"model":"gpt-5.5","input":"` + text + `"}`)
}

func TestInspectPromptFilterOpenAIDeferredStoresPendingBlockWithoutWriting(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)

	blockedBody := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")
	c, recorder := newDeferredPromptBlockContext(t, blockedBody, "window-pending-block")
	if stop := handler.inspectPromptFilterOpenAIDeferred(c, blockedBody, "/v1/responses", "gpt-5.5"); stop {
		t.Fatal("deferred inspection must not stop the request before account selection")
	}
	pending := pendingPromptBlockFromContext(c)
	if pending == nil {
		t.Fatal("blocked body left no pendingPromptBlock in the context")
	}
	if pending.evaluation.Verdict.Action != promptfilter.ActionBlock {
		t.Fatalf("pending verdict action = %q, want block", pending.evaluation.Verdict.Action)
	}
	if pending.endpoint != "/v1/responses" || pending.model != "gpt-5.5" {
		t.Fatalf("pending metadata = %+v", pending)
	}
	if recorder.Body.Len() != 0 || c.Writer.Written() {
		t.Fatalf("deferred inspection wrote a response early: %q", recorder.Body.String())
	}

	cleanBody := deferredPromptBlockBody("please summarise the meeting notes")
	cleanCtx, cleanRecorder := newDeferredPromptBlockContext(t, cleanBody, "window-pending-clean")
	if stop := handler.inspectPromptFilterOpenAIDeferred(cleanCtx, cleanBody, "/v1/responses", "gpt-5.5"); stop {
		t.Fatal("clean body must not stop the request")
	}
	if pendingPromptBlockFromContext(cleanCtx) != nil {
		t.Fatal("clean body must not leave a pendingPromptBlock")
	}
	if cleanRecorder.Body.Len() != 0 {
		t.Fatalf("clean body wrote a response: %q", cleanRecorder.Body.String())
	}
}

func TestInspectPromptFilterOpenAIDeferredKeepsWarnHeaderBehaviour(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeWarn)
	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")

	oldCtx, _ := newDeferredPromptBlockContext(t, body, "window-warn-old")
	if handler.inspectPromptFilterOpenAI(oldCtx, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("warn verdict must not block on the legacy path")
	}
	want := oldCtx.Writer.Header().Get("X-Prompt-Filter-Warning")
	if want == "" {
		t.Fatal("warn-mode config did not produce a warning header on the legacy path")
	}

	newCtx, _ := newDeferredPromptBlockContext(t, body, "window-warn-new")
	if handler.inspectPromptFilterOpenAIDeferred(newCtx, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("warn verdict must not stop the deferred path")
	}
	if got := newCtx.Writer.Header().Get("X-Prompt-Filter-Warning"); got != want {
		t.Fatalf("deferred warning header = %q, want %q", got, want)
	}
	if pendingPromptBlockFromContext(newCtx) != nil {
		t.Fatal("warn verdict must not leave a pendingPromptBlock")
	}
}

func TestEnforcePendingPromptBlockWaivesExemptAccountAndAudits(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, db := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")
	c, recorder := newDeferredPromptBlockContext(t, body, "window-exempt")
	if handler.inspectPromptFilterOpenAIDeferred(c, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("deferred inspection stopped the request early")
	}

	exempt := &auth.Account{DBID: 4242, PromptFilterPolicy: auth.PromptFilterPolicyExempt}
	if handler.enforcePendingPromptBlock(c, exempt) {
		t.Fatal("exempt account must not be blocked")
	}
	if pendingPromptBlockFromContext(c) != nil {
		t.Fatal("pending block was not cleared after the exemption")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("exempt account got a block response: %q", recorder.Body.String())
	}
	exempted, blockedAfter := PromptPolicyCounters()
	if exempted != 1 || blockedAfter != 0 {
		t.Fatalf("counters exempted=%d blocked_after_selection=%d, want 1/0", exempted, blockedAfter)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !db.WaitPromptFilterAuditIdle(waitCtx) {
		t.Fatal("prompt filter audit queue did not drain")
	}
	logs, total, err := db.ListPromptFilterLogsPage(context.Background(), database.PromptFilterLogQuery{
		Page: 1, PageSize: 10, Source: promptFilterSourceAccountExempt,
	})
	if err != nil {
		t.Fatalf("ListPromptFilterLogsPage: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("account_exempt audit rows total=%d len=%d", total, len(logs))
	}
	if logs[0].Source != promptFilterSourceAccountExempt {
		t.Fatalf("audit source = %q, want %q", logs[0].Source, promptFilterSourceAccountExempt)
	}
	if logs[0].AccountID != exempt.ID() {
		t.Fatalf("audit account_id = %d, want %d", logs[0].AccountID, exempt.ID())
	}

	// 豁免 = 放行,绝不能顺手锁死这条会话,否则同一会话的后续请求会被硬拒。
	cfg := handler.promptFilterConfigForRequest(c)
	replay, _ := newDeferredPromptBlockContext(t, body, "window-exempt")
	if _, locked := handler.activePromptConversationLock(replay, cfg, nil, "/v1/responses", "gpt-5.5"); locked {
		t.Fatal("waived request locked the conversation")
	}
}

// 豁免行意味着「判定为 block 但放行了」,并没有向客户端返回已签名的策略决策,
// 所以即使 NewAPI 签名已验证,也不能被盖成 signed_response + decision_id。
func TestEnforcePendingPromptBlockExemptAuditRowStaysUnsigned(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, db := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")
	c, recorder := newSignedDeferredPromptBlockContext(t, "exempt-signed", body, "0123456789abcdef0123456789abcdef")
	if handler.inspectPromptFilterOpenAIDeferred(c, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("deferred inspection stopped the request early")
	}
	if pendingPromptBlockFromContext(c) == nil {
		t.Fatal("signed request left no pendingPromptBlock")
	}
	exempt := &auth.Account{DBID: 909, PromptFilterPolicy: auth.PromptFilterPolicyExempt}
	if handler.enforcePendingPromptBlock(c, exempt) {
		t.Fatal("exempt account must not be blocked")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("exempt account got a block response: %q", recorder.Body.String())
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !db.WaitPromptFilterAuditIdle(waitCtx) {
		t.Fatal("prompt filter audit queue did not drain")
	}
	logs, total, err := db.ListPromptFilterLogsPage(context.Background(), database.PromptFilterLogQuery{
		Page: 1, PageSize: 10, Source: promptFilterSourceAccountExempt,
	})
	if err != nil {
		t.Fatalf("ListPromptFilterLogsPage: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("account_exempt audit rows total=%d len=%d", total, len(logs))
	}
	// 身份确实验证过(否则这条断言就白测了),但不能被改写成 signed_response。
	if logs[0].NewAPIPolicyStatus != "verified" {
		t.Fatalf("audit newapi_policy_status = %q, want verified", logs[0].NewAPIPolicyStatus)
	}
	if logs[0].NewAPIDecisionID != "" {
		t.Fatalf("waived row carries a fabricated decision id %q", logs[0].NewAPIDecisionID)
	}
	if logs[0].AccountID != exempt.ID() {
		t.Fatalf("audit account_id = %d, want %d", logs[0].AccountID, exempt.ID())
	}
}

func TestEnforcePendingPromptBlockMatchesLegacyBlockBytes(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")

	legacyCtx, legacyRecorder := newDeferredPromptBlockContext(t, body, "window-legacy-bytes")
	if !handler.inspectPromptFilterOpenAI(legacyCtx, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("legacy inspection did not block the forbidden body")
	}

	c, recorder := newDeferredPromptBlockContext(t, body, "window-deferred-bytes")
	if handler.inspectPromptFilterOpenAIDeferred(c, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("deferred inspection stopped the request early")
	}
	inherit := &auth.Account{DBID: 99}
	if !handler.enforcePendingPromptBlock(c, inherit) {
		t.Fatal("non-exempt account must be blocked after selection")
	}
	if recorder.Code != legacyRecorder.Code {
		t.Fatalf("deferred status = %d, legacy status = %d", recorder.Code, legacyRecorder.Code)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("block status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if !bytes.Equal(recorder.Body.Bytes(), legacyRecorder.Body.Bytes()) {
		t.Fatalf("deferred block body = %q, legacy block body = %q", recorder.Body.String(), legacyRecorder.Body.String())
	}
	exempted, blockedAfter := PromptPolicyCounters()
	if exempted != 0 || blockedAfter != 1 {
		t.Fatalf("counters exempted=%d blocked_after_selection=%d, want 0/1", exempted, blockedAfter)
	}

	// 与旧路径一样锁会话：同一 API Key + 窗口的后续请求必须已经处于锁定态。
	cfg := handler.promptFilterConfigForRequest(c)
	replay, _ := newDeferredPromptBlockContext(t, body, "window-deferred-bytes")
	if _, locked := handler.activePromptConversationLock(replay, cfg, nil, "/v1/responses", "gpt-5.5"); !locked {
		t.Fatal("deferred block did not lock the conversation")
	}
}

func TestEnforcePendingPromptBlockExecutesOnlyOnce(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")
	c, recorder := newDeferredPromptBlockContext(t, body, "window-once")
	if handler.inspectPromptFilterOpenAIDeferred(c, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("deferred inspection stopped the request early")
	}
	inherit := &auth.Account{DBID: 7}
	if !handler.enforcePendingPromptBlock(c, inherit) {
		t.Fatal("first enforcement must block")
	}
	firstBody := append([]byte(nil), recorder.Body.Bytes()...)
	if !handler.enforcePendingPromptBlock(c, inherit) {
		t.Fatal("second enforcement must repeat the first outcome")
	}
	if !bytes.Equal(recorder.Body.Bytes(), firstBody) {
		t.Fatalf("second enforcement wrote again: %q then %q", firstBody, recorder.Body.Bytes())
	}
	if _, blockedAfter := PromptPolicyCounters(); blockedAfter != 1 {
		t.Fatalf("blocked_after_selection = %d, want 1", blockedAfter)
	}
}

func TestEnforcePendingPromptBlockWithoutPendingBlockIsInert(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	body := deferredPromptBlockBody("please summarise the meeting notes")
	c, recorder := newDeferredPromptBlockContext(t, body, "window-inert")
	if handler.inspectPromptFilterOpenAIDeferred(c, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("clean body stopped the request")
	}
	if handler.enforcePendingPromptBlock(c, &auth.Account{DBID: 1}) {
		t.Fatal("enforcement without a pending block must not stop the request")
	}
	if recorder.Body.Len() != 0 || c.Writer.Written() {
		t.Fatalf("enforcement without a pending block wrote a response: %q", recorder.Body.String())
	}
	exempted, blockedAfter := PromptPolicyCounters()
	if exempted != 0 || blockedAfter != 0 {
		t.Fatalf("counters exempted=%d blocked_after_selection=%d, want 0/0", exempted, blockedAfter)
	}
}

func TestSessionGuardStatusSnapshotExposesPromptPolicyCounters(t *testing.T) {
	resetPromptPolicyCounters(t)
	promptPolicyExempted.Add(3)
	promptPolicyBlockedAfterSelection.Add(5)
	status := SessionGuardStatusSnapshot(nil)
	if status.PromptPolicy.Exempted != 3 || status.PromptPolicy.BlockedAfterSelection != 5 {
		t.Fatalf("prompt policy snapshot = %+v, want 3/5", status.PromptPolicy)
	}
}

// readDeferredWSFrame 读取一帧;超时即判定为「没写」。
func readDeferredWSFrame(t *testing.T, client *websocket.Conn) ([]byte, bool) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, payload, err := client.ReadMessage()
	if err != nil {
		return nil, false
	}
	return payload, true
}

func TestEnforcePendingPromptBlockWSWaivesExemptAccount(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	serverConn, client, cleanup := newDownstreamWSPair(t)
	defer cleanup()

	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")
	c, _ := newDeferredPromptBlockContext(t, body, "window-ws-exempt")
	blocked, delegated := handler.inspectPromptFilterOpenAIForWebSocketDeferred(c, serverConn, body, "/v1/responses", "gpt-5.5", "responses:1")
	if blocked || delegated {
		t.Fatalf("deferred WS inspection stopped the turn early: blocked=%v delegated=%v", blocked, delegated)
	}
	if pendingPromptBlockFromContext(c) == nil {
		t.Fatal("blocked WS turn left no pendingPromptBlock")
	}
	if payload, ok := readDeferredWSFrame(t, client); ok {
		t.Fatalf("deferred WS inspection wrote a frame early: %s", payload)
	}

	exempt := &auth.Account{DBID: 4242, PromptFilterPolicy: auth.PromptFilterPolicyExempt}
	blocked, delegated = handler.enforcePendingPromptBlockWS(c, serverConn, exempt, "responses:1")
	if blocked || delegated {
		t.Fatalf("exempt account was blocked: blocked=%v delegated=%v", blocked, delegated)
	}
	if pendingPromptBlockFromContext(c) != nil {
		t.Fatal("pending block was not cleared after the WS exemption")
	}
	// 放行 = 这一轮继续转发给上游,网关不写任何错误帧。
	if payload, ok := readDeferredWSFrame(t, client); ok {
		t.Fatalf("waived WS turn wrote an error frame: %s", payload)
	}
	exempted, blockedAfter := PromptPolicyCounters()
	if exempted != 1 || blockedAfter != 0 {
		t.Fatalf("counters exempted=%d blocked_after_selection=%d, want 1/0", exempted, blockedAfter)
	}
}

func TestEnforcePendingPromptBlockWSMatchesLegacyFrame(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")

	legacyConn, legacyClient, legacyCleanup := newDownstreamWSPair(t)
	defer legacyCleanup()
	legacyCtx, _ := newDeferredPromptBlockContext(t, body, "window-ws-legacy")
	blocked, delegated := handler.inspectPromptFilterOpenAIForWebSocket(legacyCtx, legacyConn, body, "/v1/responses", "gpt-5.5", "responses:1")
	if !blocked || delegated {
		t.Fatalf("legacy WS inspection blocked=%v delegated=%v, want true/false", blocked, delegated)
	}
	legacyFrame, ok := readDeferredWSFrame(t, legacyClient)
	if !ok {
		t.Fatal("legacy WS inspection wrote no error frame")
	}

	serverConn, client, cleanup := newDownstreamWSPair(t)
	defer cleanup()
	c, _ := newDeferredPromptBlockContext(t, body, "window-ws-deferred")
	if blocked, delegated = handler.inspectPromptFilterOpenAIForWebSocketDeferred(c, serverConn, body, "/v1/responses", "gpt-5.5", "responses:1"); blocked || delegated {
		t.Fatalf("deferred WS inspection stopped the turn early: blocked=%v delegated=%v", blocked, delegated)
	}
	inherit := &auth.Account{DBID: 99}
	blocked, delegated = handler.enforcePendingPromptBlockWS(c, serverConn, inherit, "responses:1")
	if !blocked || delegated {
		t.Fatalf("deferred WS enforcement blocked=%v delegated=%v, want true/false", blocked, delegated)
	}
	frame, ok := readDeferredWSFrame(t, client)
	if !ok {
		t.Fatal("deferred WS enforcement wrote no error frame")
	}
	if !bytes.Equal(frame, legacyFrame) {
		t.Fatalf("deferred frame = %s, legacy frame = %s", frame, legacyFrame)
	}
	if _, blockedAfter := PromptPolicyCounters(); blockedAfter != 1 {
		t.Fatalf("blocked_after_selection = %d, want 1", blockedAfter)
	}

	// 重复执行只写一次,并复述第一次的 (blocked, delegated)。
	if blocked, delegated = handler.enforcePendingPromptBlockWS(c, serverConn, inherit, "responses:1"); !blocked || delegated {
		t.Fatalf("second WS enforcement = (%v, %v), want (true, false)", blocked, delegated)
	}
	if payload, ok := readDeferredWSFrame(t, client); ok {
		t.Fatalf("second WS enforcement wrote again: %s", payload)
	}
}

// NewAPI 签名已验证时拦截由平台决策接管,delegated 必须为 true,且与旧路径一样
// 先写出决策响应头。决策 ID 随请求变化,所以这里比对语义而非字节。
func TestEnforcePendingPromptBlockWSDelegatesVerifiedNewAPIDecision(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	body := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")

	legacyConn, legacyClient, legacyCleanup := newDownstreamWSPair(t)
	defer legacyCleanup()
	legacyCtx := newSignedDeferredPromptBlockWSContext(t, "ws-legacy-signed", "0123456789abcdef0123456789abcdef")
	blocked, delegated := handler.inspectPromptFilterOpenAIForWebSocket(legacyCtx, legacyConn, body, "/v1/responses", "gpt-5.5", "responses:1")
	if !blocked || !delegated {
		t.Fatalf("legacy verified WS inspection = (%v, %v), want (true, true)", blocked, delegated)
	}
	legacyFrame, ok := readDeferredWSFrame(t, legacyClient)
	if !ok {
		t.Fatal("legacy verified WS inspection wrote no error frame")
	}

	serverConn, client, cleanup := newDownstreamWSPair(t)
	defer cleanup()
	c := newSignedDeferredPromptBlockWSContext(t, "ws-deferred-signed", "fedcba9876543210fedcba9876543210")
	if blocked, delegated = handler.inspectPromptFilterOpenAIForWebSocketDeferred(c, serverConn, body, "/v1/responses", "gpt-5.5", "responses:1"); blocked || delegated {
		t.Fatalf("deferred verified WS inspection stopped the turn early: blocked=%v delegated=%v", blocked, delegated)
	}
	inherit := &auth.Account{DBID: 99}
	blocked, delegated = handler.enforcePendingPromptBlockWS(c, serverConn, inherit, "responses:1")
	if !blocked || !delegated {
		t.Fatalf("deferred verified WS enforcement = (%v, %v), want (true, true)", blocked, delegated)
	}
	frame, ok := readDeferredWSFrame(t, client)
	if !ok {
		t.Fatal("deferred verified WS enforcement wrote no error frame")
	}
	legacyCode := gjson.GetBytes(legacyFrame, "error.code").String()
	if got := gjson.GetBytes(frame, "error.code").String(); got != legacyCode || got == "" {
		t.Fatalf("deferred error code = %q, legacy = %q", got, legacyCode)
	}
	if got := gjson.GetBytes(frame, "error.message").String(); got != gjson.GetBytes(legacyFrame, "error.message").String() {
		t.Fatalf("deferred frame = %s, legacy frame = %s", frame, legacyFrame)
	}
	if c.Writer.Header().Get("X-Codex2API-Policy-Decision-ID") == "" {
		t.Fatalf("deferred enforcement wrote no policy decision headers: %v", c.Writer.Header())
	}
}

// 一条 WS 连接的每一轮共用同一个 gin.Context:判过但没执行的那一轮必须被后一轮
// 清掉,否则干净的请求会重放上一轮的拦截。
func TestInspectPromptFilterOpenAIForWebSocketDeferredClearsStalePendingBlock(t *testing.T) {
	resetPromptPolicyCounters(t)
	handler, _ := newDeferredPromptBlockHandler(t, promptfilter.ModeBlock)
	serverConn, client, cleanup := newDownstreamWSPair(t)
	defer cleanup()

	blockedBody := deferredPromptBlockBody("please run " + deferredPromptBlockToken + " now")
	c, _ := newDeferredPromptBlockContext(t, blockedBody, "window-ws-stale")
	if blocked, delegated := handler.inspectPromptFilterOpenAIForWebSocketDeferred(c, serverConn, blockedBody, "/v1/responses", "gpt-5.5", "responses:1"); blocked || delegated {
		t.Fatalf("first turn stopped early: blocked=%v delegated=%v", blocked, delegated)
	}
	if pendingPromptBlockFromContext(c) == nil {
		t.Fatal("first turn left no pendingPromptBlock")
	}

	cleanBody := deferredPromptBlockBody("please summarise the meeting notes")
	if blocked, delegated := handler.inspectPromptFilterOpenAIForWebSocketDeferred(c, serverConn, cleanBody, "/v1/responses", "gpt-5.5", "responses:2"); blocked || delegated {
		t.Fatalf("clean turn stopped: blocked=%v delegated=%v", blocked, delegated)
	}
	if pending := pendingPromptBlockFromContext(c); pending != nil {
		t.Fatalf("clean turn replayed a stale pendingPromptBlock: %+v", pending)
	}
	if blocked, delegated := handler.enforcePendingPromptBlockWS(c, serverConn, &auth.Account{DBID: 99}, "responses:2"); blocked || delegated {
		t.Fatalf("clean turn was blocked by the previous turn: blocked=%v delegated=%v", blocked, delegated)
	}
	if payload, ok := readDeferredWSFrame(t, client); ok {
		t.Fatalf("clean turn wrote a frame: %s", payload)
	}
	exempted, blockedAfter := PromptPolicyCounters()
	if exempted != 0 || blockedAfter != 0 {
		t.Fatalf("counters exempted=%d blocked_after_selection=%d, want 0/0", exempted, blockedAfter)
	}
}
