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

func deferredPromptBlockBody(text string) []byte {
	return []byte(`{"model":"gpt-5.5","input":"` + text + `"}`)
}

func TestInspectPromptFilterOpenAIDeferredStoresPendingBlockWithoutWriting(t *testing.T) {
	resetPromptPolicyCountersForTest()
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
	if pending.endpoint != "/v1/responses" || pending.model != "gpt-5.5" || pending.transport != promptfilter.TransportHTTP {
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
	resetPromptPolicyCountersForTest()
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
	resetPromptPolicyCountersForTest()
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
}

func TestEnforcePendingPromptBlockMatchesLegacyBlockBytes(t *testing.T) {
	resetPromptPolicyCountersForTest()
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
	resetPromptPolicyCountersForTest()
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
	resetPromptPolicyCountersForTest()
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
	resetPromptPolicyCountersForTest()
	promptPolicyExempted.Add(3)
	promptPolicyBlockedAfterSelection.Add(5)
	t.Cleanup(resetPromptPolicyCountersForTest)
	status := SessionGuardStatusSnapshot(nil)
	if status.PromptPolicy.Exempted != 3 || status.PromptPolicy.BlockedAfterSelection != 5 {
		t.Fatalf("prompt policy snapshot = %+v, want 3/5", status.PromptPolicy)
	}
}
