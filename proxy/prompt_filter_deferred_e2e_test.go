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
			if _, blockedAfter := PromptPolicyCounters(); blockedAfter != 1 {
				t.Fatalf("blocked_after_selection = %d, want 1", blockedAfter)
			}
		})
	}
}

type deferredWSE2EResult struct {
	frames       [][]byte
	turnErr      error
	upstreamHits int32
	account      *auth.Account
}

// runDeferredPromptBlockWSTurn 把一轮 WS 请求跑完整：桩掉上游 WS 执行函数，
// 这样「有没有连上游」与「下游收到什么帧」都能直接断言。
func runDeferredPromptBlockWSTurn(t *testing.T, text string, policy string) deferredWSE2EResult {
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
	account := &auth.Account{
		DBID: 1, AccessToken: "deferred-ws-token", AccountID: "deferred-ws", PlanType: "plus",
		Models: []string{deferredE2EModel}, PromptFilterPolicy: policy,
	}
	store.AddAccount(account)

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	turnErrs := make(chan error, 1)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		conn, upgradeErr := responsesWSUpgrader.Upgrade(c.Writer, c.Request, nil)
		if upgradeErr != nil {
			turnErrs <- upgradeErr
			return
		}
		defer conn.Close()
		_, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			turnErrs <- readErr
			return
		}
		turnErrs <- handler.forwardResponsesWebSocketTurn(c, conn, payload, "deferred-ws-turn", nil)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	client, _, dialErr := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http"),
		http.Header{"Session-Id": []string{"deferred-ws-session"}},
	)
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}
	defer client.Close()

	turn := `{"type":"response.create","model":"` + deferredE2EModel + `","input":"` + text + `"}`
	if writeErr := client.WriteMessage(websocket.TextMessage, []byte(turn)); writeErr != nil {
		t.Fatalf("write turn: %v", writeErr)
	}

	var frames [][]byte
	for {
		payload, ok := readDeferredWSFrame(t, client)
		if !ok {
			break
		}
		frames = append(frames, payload)
	}
	var turnErr error
	select {
	case turnErr = <-turnErrs:
	case <-time.After(2 * time.Second):
		t.Fatal("WS turn did not finish")
	}
	return deferredWSE2EResult{frames: frames, turnErr: turnErr, upstreamHits: atomic.LoadInt32(&hits), account: account}
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
	result := runDeferredPromptBlockWSTurn(t, "please run "+deferredPromptBlockToken+" now", auth.PromptFilterPolicyExempt)
	if result.turnErr != nil {
		t.Fatalf("豁免账号的 WS 轮次出错: %v", result.turnErr)
	}
	if result.upstreamHits != 1 {
		t.Fatalf("上游命中 = %d, want 1", result.upstreamHits)
	}
	if deferredWSFrameWithCode(result.frames, "prompt_blocked") {
		t.Fatalf("豁免账号仍收到拦截帧: %s", result.frames)
	}
	if exempted, blockedAfter := PromptPolicyCounters(); exempted != 1 || blockedAfter != 0 {
		t.Fatalf("计数 exempted=%d blocked_after_selection=%d, want 1/0", exempted, blockedAfter)
	}
}

func TestDeferredPromptBlockWSE2EInheritAccountBlocksAfterSelection(t *testing.T) {
	resetPromptPolicyCounters(t)
	result := runDeferredPromptBlockWSTurn(t, "please run "+deferredPromptBlockToken+" now", auth.PolicyInherit)
	if result.turnErr == nil {
		t.Fatal("被拦的 WS 轮次必须以关闭错误结束")
	}
	if !deferredWSFrameWithCode(result.frames, "prompt_blocked") {
		t.Fatalf("没有收到 prompt_blocked 错误帧: %s", result.frames)
	}
	if result.upstreamHits != 0 {
		t.Fatalf("被拦的 WS 轮次连了上游 %d 次", result.upstreamHits)
	}
	if active := result.account.GetActiveRequests(); active != 0 {
		t.Fatalf("账号并发 = %d, want 0（WS 拦截分支没有 Release）", active)
	}
	if exempted, blockedAfter := PromptPolicyCounters(); exempted != 0 || blockedAfter != 1 {
		t.Fatalf("计数 exempted=%d blocked_after_selection=%d, want 0/1", exempted, blockedAfter)
	}
}
