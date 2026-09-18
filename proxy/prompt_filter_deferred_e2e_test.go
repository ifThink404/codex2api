package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// 「先判后拦」的端到端账：真实 handler + 真实路由 + 桩上游。判定发生在选号前，
// 但拦不拦要等选到账号才知道——所以这里只认两件事：上游到底有没有被打到，
// 下游到底收到了什么。中间状态一概不看。
const deferredE2EModel = "gpt-5.5"

func deferredE2ESystemSettings() *database.SystemSettings {
	return &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0,
		PromptFilterEnabled:          true,
		PromptFilterMode:             promptfilter.ModeBlock,
		PromptFilterThreshold:        50,
		PromptFilterStrictThreshold:  90,
		PromptFilterLogMatches:       true,
		PromptFilterMaxTextLength:    promptfilter.DefaultMaxTextLength,
		PromptFilterCustomPatterns:   `[{"name":"deferred_block_token","pattern":"forbidden_token","weight":100,"strict":true,"category":"test"}]`,
		PromptFilterDisabledPatterns: "[]",
	}
}

type deferredE2ECase struct {
	name string
	path string
	// body 用 %s 占位承载被检测的那段文本。
	body        string
	contentType string
	payload     string
	// blockedStatus/blockedCodePath 是被拦住时下游应当看到的状态码与错误码位置：
	// /v1/messages 保留 Anthropic 自己的错误信封。
	blockedStatus   int
	blockedCodePath string
	blockedCode     string
}

func deferredE2ECases() []deferredE2ECase {
	return []deferredE2ECase{
		{
			name: "responses", path: "/v1/responses",
			body:        `{"model":"` + deferredE2EModel + `","input":"%s","stream":false}`,
			contentType: "application/json", payload: coverageResponsesJSON,
			blockedStatus: http.StatusBadRequest, blockedCodePath: "error.code", blockedCode: "prompt_blocked",
		},
		{
			name: "compact", path: "/v1/responses/compact",
			body:        `{"model":"` + deferredE2EModel + `","input":"%s"}`,
			contentType: "application/json", payload: coverageResponsesJSON,
			blockedStatus: http.StatusBadRequest, blockedCodePath: "error.code", blockedCode: "prompt_blocked",
		},
		{
			name: "chat", path: "/v1/chat/completions",
			body:        `{"model":"` + deferredE2EModel + `","messages":[{"role":"user","content":"%s"}],"stream":false}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			blockedStatus: http.StatusBadRequest, blockedCodePath: "error.code", blockedCode: "prompt_blocked",
		},
		{
			name: "messages", path: "/v1/messages",
			body:        `{"model":"` + deferredE2EModel + `","max_tokens":64,"messages":[{"role":"user","content":"%s"}],"stream":false}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			blockedStatus: http.StatusBadRequest, blockedCodePath: "error.type", blockedCode: "invalid_request_error",
		},
	}
}

type deferredE2EResult struct {
	status       int
	body         string
	upstreamHits int32
	account      *auth.Account
	store        *auth.Store
	affinityKey  string
}

func runDeferredPromptBlockE2E(t *testing.T, tc deferredE2ECase, text string, policy string, withAccount bool) deferredE2EResult {
	t.Helper()
	gin.SetMode(gin.TestMode)

	previousSettings := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", tc.contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, tc.payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "deferred-prompt-block-e2e.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := auth.NewStore(nil, nil, deferredE2ESystemSettings())
	t.Cleanup(store.Stop)
	account := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL,
		APIKey: "deferred-e2e", Models: []string{deferredE2EModel}, PlanType: "api",
		PromptFilterPolicy: policy,
	}
	if withAccount {
		store.AddAccount(account)
	}

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)

	body := strings.Replace(tc.body, "%s", text, 1)
	request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	identity := resolveRequestSessionIdentity(request.Header, []byte(body))
	return deferredE2EResult{
		status:       recorder.Code,
		body:         recorder.Body.String(),
		upstreamHits: atomic.LoadInt32(&hits),
		account:      account,
		store:        store,
		affinityKey:  sessionAffinityKey(identity.affinityID, 0),
	}
}

// 豁免账号：命中本地规则的请求照样打到上游，下游拿到正常回包。
func TestDeferredPromptBlockE2EExemptAccountReachesUpstream(t *testing.T) {
	for _, tc := range deferredE2ECases() {
		t.Run(tc.name, func(t *testing.T) {
			resetPromptPolicyCounters(t)
			result := runDeferredPromptBlockE2E(t,
				tc, "please run "+deferredPromptBlockToken+" now", auth.PromptFilterPolicyExempt, true)
			if result.status != http.StatusOK {
				t.Fatalf("状态 = %d, want 200; body=%s", result.status, result.body)
			}
			if result.upstreamHits != 1 {
				t.Fatalf("上游命中 = %d, want 1; body=%s", result.upstreamHits, result.body)
			}
			if exempted, blockedAfter := PromptPolicyCounters(); exempted != 1 || blockedAfter != 0 {
				t.Fatalf("计数 exempted=%d blocked_after_selection=%d, want 1/0", exempted, blockedAfter)
			}
		})
	}
}

// inherit 账号：拦截照旧，且上游一次都不能被打到，账号并发与会话亲和必须归位。
func TestDeferredPromptBlockE2EInheritAccountBlocksAfterSelection(t *testing.T) {
	for _, tc := range deferredE2ECases() {
		t.Run(tc.name, func(t *testing.T) {
			resetPromptPolicyCounters(t)
			result := runDeferredPromptBlockE2E(t,
				tc, "please run "+deferredPromptBlockToken+" now", auth.PolicyInherit, true)
			if result.status != tc.blockedStatus {
				t.Fatalf("状态 = %d, want %d; body=%s", result.status, tc.blockedStatus, result.body)
			}
			if got := gjson.Get(result.body, tc.blockedCodePath).String(); got != tc.blockedCode {
				t.Fatalf("%s = %q, want %q; body=%s", tc.blockedCodePath, got, tc.blockedCode, result.body)
			}
			// /v1/messages 的 Anthropic 信封没有 error.code，用文案守住区分度。
			if got := gjson.Get(result.body, "error.message").String(); got != defaultLocalPromptBlockMessage {
				t.Fatalf("error.message = %q, want %q; body=%s", got, defaultLocalPromptBlockMessage, result.body)
			}
			if result.upstreamHits != 0 {
				t.Fatalf("被拦的请求打到了上游 %d 次", result.upstreamHits)
			}
			if active := result.account.GetActiveRequests(); active != 0 {
				t.Fatalf("账号并发 = %d, want 0（拦截分支没有 Release）", active)
			}
			if boundID, ok := result.store.SessionAffinityAccountID(result.affinityKey); ok {
				t.Fatalf("会话亲和仍绑在账号 %d 上（拦截分支没有 Unbind）", boundID)
			}
			if exempted, blockedAfter := PromptPolicyCounters(); exempted != 0 || blockedAfter != 1 {
				t.Fatalf("计数 exempted=%d blocked_after_selection=%d, want 0/1", exempted, blockedAfter)
			}
		})
	}
}

// 干净请求走 inherit 账号：一切照旧，不留任何待执行拦截。
func TestDeferredPromptBlockE2ECleanRequestUnchanged(t *testing.T) {
	for _, tc := range deferredE2ECases() {
		t.Run(tc.name, func(t *testing.T) {
			resetPromptPolicyCounters(t)
			result := runDeferredPromptBlockE2E(t, tc, "please summarise the meeting notes", auth.PolicyInherit, true)
			if result.status != http.StatusOK {
				t.Fatalf("状态 = %d, want 200; body=%s", result.status, result.body)
			}
			if result.upstreamHits != 1 {
				t.Fatalf("上游命中 = %d, want 1; body=%s", result.upstreamHits, result.body)
			}
			if exempted, blockedAfter := PromptPolicyCounters(); exempted != 0 || blockedAfter != 0 {
				t.Fatalf("计数 exempted=%d blocked_after_selection=%d, want 0/0", exempted, blockedAfter)
			}
		})
	}
}

// 号池全空：选不到账号 = 没有账号能豁免，下游必须拿到拦截响应，
// 而不是一句误导性的「无可用账号」。
func TestDeferredPromptBlockE2EEmptyPoolStillBlocks(t *testing.T) {
	for _, tc := range deferredE2ECases() {
		t.Run(tc.name, func(t *testing.T) {
			resetPromptPolicyCounters(t)
			result := runDeferredPromptBlockE2E(t,
				tc, "please run "+deferredPromptBlockToken+" now", auth.PolicyInherit, false)
			if result.status != tc.blockedStatus {
				t.Fatalf("状态 = %d, want %d; body=%s", result.status, tc.blockedStatus, result.body)
			}
			if got := gjson.Get(result.body, tc.blockedCodePath).String(); got != tc.blockedCode {
				t.Fatalf("%s = %q, want %q; body=%s", tc.blockedCodePath, got, tc.blockedCode, result.body)
			}
			if got := gjson.Get(result.body, "error.message").String(); got != defaultLocalPromptBlockMessage {
				t.Fatalf("error.message = %q, want %q; body=%s", got, defaultLocalPromptBlockMessage, result.body)
			}
			if _, blockedAfter := PromptPolicyCounters(); blockedAfter != 1 {
				t.Fatalf("blocked_after_selection = %d, want 1", blockedAfter)
			}
		})
	}
}

// 校验失败与命中本地规则同时成立时：拦截优先。改动前 inspect 跑在校验之前，
// 客户端只要在命中的请求里多带一个非法字段（tools[0].name=""），
// 就能拿到 invalid_parameter 而不是 prompt_blocked，会话锁与 NewAPI 决策一并丢掉。
func TestDeferredPromptBlockE2EMalformedRequestStillBlocksAndLocks(t *testing.T) {
	resetPromptPolicyCounters(t)
	gin.SetMode(gin.TestMode)

	previousSettings := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, coverageResponsesJSON)
	}))
	t.Cleanup(upstream.Close)

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "deferred-prompt-lock-e2e.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := auth.NewStore(nil, nil, deferredE2ESystemSettings())
	t.Cleanup(store.Stop)
	cfg := store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.ConversationLockEnabled = true
	store.SetPromptFilterConfig(cfg)
	store.AddAccount(&auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL,
		APIKey: "deferred-lock-e2e", Models: []string{deferredE2EModel}, PlanType: "api",
		PromptFilterPolicy: auth.PolicyInherit,
	})

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	// 会话锁的降级身份要求非 0 的下游 API Key（promptConversationLockFallbackIdentity）。
	router.Use(func(c *gin.Context) { c.Set(contextAPIKeyID, int64(7)) })
	handler.RegisterRoutes(router)

	send := func(sessionID string, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Session-Id", sessionID)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	malformed := `{"model":"` + deferredE2EModel + `","input":"please run ` + deferredPromptBlockToken +
		` now","tools":[{"type":"function","name":""}],"stream":false}`
	blocked := send("deferred-lock-session", malformed)
	if blocked.Code != http.StatusBadRequest {
		t.Fatalf("状态 = %d, want 400; body=%s", blocked.Code, blocked.Body.String())
	}
	if got := gjson.Get(blocked.Body.String(), "error.code").String(); got != "prompt_blocked" {
		t.Fatalf("error.code = %q, want prompt_blocked; body=%s", got, blocked.Body.String())
	}
	if _, blockedAfter := PromptPolicyCounters(); blockedAfter != 1 {
		t.Fatalf("blocked_after_selection = %d, want 1", blockedAfter)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("被拦的请求打到了上游 %d 次", atomic.LoadInt32(&hits))
	}

	// 同一会话的干净请求必须被会话锁拦下——锁是本地 block 在发往上游前就要写的。
	clean := `{"model":"` + deferredE2EModel + `","input":"please summarise the meeting notes","stream":false}`
	locked := send("deferred-lock-session", clean)
	if locked.Code != http.StatusBadRequest {
		t.Fatalf("已锁定会话的后续请求状态 = %d, want 400; body=%s", locked.Code, locked.Body.String())
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("会话锁没有写下：后续请求打到上游 %d 次", atomic.LoadInt32(&hits))
	}

	// 对照组：另一个会话的同一条干净请求照常到达上游，证明锁是按会话的，
	// 也证明这个 harness 本来就能打通上游。
	other := send("deferred-lock-other-session", clean)
	if other.Code != http.StatusOK {
		t.Fatalf("无关会话状态 = %d, want 200; body=%s", other.Code, other.Body.String())
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("无关会话的上游命中 = %d, want 1", atomic.LoadInt32(&hits))
	}
}

// deferredWSE2EOptions 描述一次 WS 端到端跑法：一条连接上按顺序跑若干轮。
type deferredWSE2EOptions struct {
	policy      string
	apiKeyID    int64
	lockEnabled bool
	sessionID   string
	payloads    []string
}

type deferredWSE2ETurn struct {
	frames  [][]byte
	turnErr error
}

type deferredWSE2ERun struct {
	turns        []deferredWSE2ETurn
	upstreamHits int32
	account      *auth.Account
	store        *auth.Store
	affinityKey  string
}

// deferredWSTurnPayload 拼一条 response.create；extra 原样插进对象里，
// 用来构造「命中规则 + 请求本身非法」的组合。
func deferredWSTurnPayload(text string, extra string) string {
	payload := `{"type":"response.create","model":"` + deferredE2EModel + `","input":"` + text + `"`
	if extra != "" {
		payload += "," + extra
	}
	return payload + "}"
}

// runDeferredPromptBlockWSTurns 把若干轮 WS 请求跑完整：桩掉上游 WS 执行函数，
// 这样「有没有连上游」与「下游收到什么帧」都能直接断言。
func runDeferredPromptBlockWSTurns(t *testing.T, opts deferredWSE2EOptions) deferredWSE2ERun {
	t.Helper()
	gin.SetMode(gin.TestMode)

	previousSettings := CurrentRuntimeSettings()
	ApplyRuntimeSettings(DefaultRuntimeSettings())
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	var hits int32
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		atomic.AddInt32(&hits, 1)
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(wsContextTestSSE("resp_deferred_ws"))),
		}, nil
	}

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "deferred-prompt-block-ws-e2e.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := auth.NewStore(nil, nil, deferredE2ESystemSettings())
	t.Cleanup(store.Stop)
	if opts.lockEnabled {
		cfg := store.GetPromptFilterConfig()
		cfg.Advanced.Enforcement.ConversationLockEnabled = true
		store.SetPromptFilterConfig(cfg)
	}
	account := &auth.Account{
		DBID: 1, AccessToken: "deferred-ws-token", AccountID: "deferred-ws", PlanType: "plus",
		Models: []string{deferredE2EModel}, PromptFilterPolicy: opts.policy,
	}
	store.AddAccount(account)

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	turnErrs := make(chan error, len(opts.payloads))
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		if opts.apiKeyID != 0 {
			// 会话锁的降级身份要求非 0 的下游 API Key（promptConversationLockFallbackIdentity）。
			c.Set(contextAPIKeyID, opts.apiKeyID)
		}
		conn, upgradeErr := responsesWSUpgrader.Upgrade(c.Writer, c.Request, nil)
		if upgradeErr != nil {
			turnErrs <- upgradeErr
			return
		}
		defer conn.Close()
		for range opts.payloads {
			_, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				turnErrs <- readErr
				return
			}
			turnErrs <- handler.forwardResponsesWebSocketTurn(c, conn, payload, "deferred-ws-turn", nil)
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()

	sessionID := opts.sessionID
	if sessionID == "" {
		sessionID = "deferred-ws-session"
	}
	dialHeader := http.Header{"Session-Id": []string{sessionID}}
	client, _, dialErr := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), dialHeader)
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}
	defer client.Close()

	run := deferredWSE2ERun{account: account, store: store}
	for index, payload := range opts.payloads {
		if writeErr := client.WriteMessage(websocket.TextMessage, []byte(payload)); writeErr != nil {
			t.Fatalf("write turn %d: %v", index, writeErr)
		}
		var frames [][]byte
		for {
			frame, ok := readDeferredWSFrame(t, client)
			if !ok {
				break
			}
			frames = append(frames, frame)
		}
		select {
		case turnErr := <-turnErrs:
			run.turns = append(run.turns, deferredWSE2ETurn{frames: frames, turnErr: turnErr})
		case <-time.After(2 * time.Second):
			t.Fatalf("WS turn %d did not finish", index)
		}
	}
	identity := resolveRequestSessionIdentity(dialHeader, []byte(opts.payloads[0]))
	run.affinityKey = sessionAffinityKey(identity.affinityID, opts.apiKeyID)
	run.upstreamHits = atomic.LoadInt32(&hits)
	return run
}

func runDeferredPromptBlockWSTurn(t *testing.T, text string, policy string) deferredWSE2ERun {
	t.Helper()
	return runDeferredPromptBlockWSTurns(t, deferredWSE2EOptions{
		policy:   policy,
		payloads: []string{deferredWSTurnPayload(text, "")},
	})
}

func deferredWSFrameWithCode(frames [][]byte, code string) bool {
	for _, frame := range frames {
		if gjson.GetBytes(frame, "error.code").String() == code {
			return true
		}
	}
	return false
}

func TestDeferredPromptBlockWSE2EExemptAccountForwardsTurn(t *testing.T) {
	resetPromptPolicyCounters(t)
	run := runDeferredPromptBlockWSTurn(t, "please run "+deferredPromptBlockToken+" now", auth.PromptFilterPolicyExempt)
	turn := run.turns[0]
	if turn.turnErr != nil {
		t.Fatalf("豁免账号的 WS 轮次出错: %v", turn.turnErr)
	}
	if run.upstreamHits != 1 {
		t.Fatalf("上游命中 = %d, want 1", run.upstreamHits)
	}
	if deferredWSFrameWithCode(turn.frames, "prompt_blocked") {
		t.Fatalf("豁免账号仍收到拦截帧: %s", turn.frames)
	}
	if exempted, blockedAfter := PromptPolicyCounters(); exempted != 1 || blockedAfter != 0 {
		t.Fatalf("计数 exempted=%d blocked_after_selection=%d, want 1/0", exempted, blockedAfter)
	}
}

func TestDeferredPromptBlockWSE2EInheritAccountBlocksAfterSelection(t *testing.T) {
	resetPromptPolicyCounters(t)
	run := runDeferredPromptBlockWSTurn(t, "please run "+deferredPromptBlockToken+" now", auth.PolicyInherit)
	turn := run.turns[0]
	if turn.turnErr == nil {
		t.Fatal("被拦的 WS 轮次必须以关闭错误结束")
	}
	if !deferredWSFrameWithCode(turn.frames, "prompt_blocked") {
		t.Fatalf("没有收到 prompt_blocked 错误帧: %s", turn.frames)
	}
	if run.upstreamHits != 0 {
		t.Fatalf("被拦的 WS 轮次连了上游 %d 次", run.upstreamHits)
	}
	if active := run.account.GetActiveRequests(); active != 0 {
		t.Fatalf("账号并发 = %d, want 0（WS 拦截分支没有 Release）", active)
	}
	if boundID, ok := run.store.SessionAffinityAccountID(run.affinityKey); ok {
		t.Fatalf("会话亲和仍绑在账号 %d 上（WS 拦截分支没有 Unbind）", boundID)
	}
	if exempted, blockedAfter := PromptPolicyCounters(); exempted != 0 || blockedAfter != 1 {
		t.Fatalf("计数 exempted=%d blocked_after_selection=%d, want 0/1", exempted, blockedAfter)
	}
}

// WS 版的「校验失败 + 命中规则」：拦截优先，且会话锁必须在这一轮就写下——
// 否则下一条绕过正则的变形会被放行到上游。
func TestDeferredPromptBlockWSE2EMalformedTurnStillBlocksAndLocks(t *testing.T) {
	resetPromptPolicyCounters(t)
	malformed := deferredWSTurnPayload(
		"please run "+deferredPromptBlockToken+" now", `"tools":[{"type":"function","name":""}]`)
	clean := deferredWSTurnPayload("please summarise the meeting notes", "")
	run := runDeferredPromptBlockWSTurns(t, deferredWSE2EOptions{
		policy: auth.PolicyInherit, apiKeyID: 7, lockEnabled: true,
		sessionID: "deferred-ws-lock-session",
		payloads:  []string{malformed, clean},
	})
	if !deferredWSFrameWithCode(run.turns[0].frames, "prompt_blocked") {
		t.Fatalf("畸形 + 命中的轮次没有收到 prompt_blocked 帧: %s", run.turns[0].frames)
	}
	if _, blockedAfter := PromptPolicyCounters(); blockedAfter != 1 {
		t.Fatalf("blocked_after_selection = %d, want 1", blockedAfter)
	}
	// 第二轮是干净请求：没有会话锁它会被放行到上游。
	if run.upstreamHits != 0 {
		t.Fatalf("会话锁没有写下：后续请求打到上游 %d 次", run.upstreamHits)
	}
	if run.turns[1].turnErr == nil {
		t.Fatal("已锁定会话的后续轮次必须被拦下")
	}
}
