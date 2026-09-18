package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 上游自报模型的分支覆盖：每一条「真的消费了上游响应信封」的日志行都必须带上
// 上游自己声明的模型。这里用真实 handler + 桩上游把每个分支各跑一遍，断言落库的
// UpstreamResponseModel 等于桩里写的模型名——只验字段本身，不验其余计费字段。
//
// 桩里的模型名刻意与请求模型不同：如果哪个分支退化成「回填请求模型」，断言会立刻
// 失败，而不是因为两者碰巧相等而蒙混过关。
const (
	coverageUpstreamResponsesModel = "upstream-responses-1"
	coverageUpstreamChatModel      = "upstream-chat-1"
	coverageUpstreamClaudeModel    = "claude-upstream-1"

	// 窗口号只对官方 Codex OAuth 账号记录（运营决定 2026-09-18）：中转 / Grok /
	// Antigravity / Claude 账号即便收到同一个请求头也必须留空。
	coverageWindowID     = "0199a2b0-1f3c-7c11-8f2e-4b6d9a1c2e30:4"
	coverageWindowNumber = "4"
)

const coverageResponsesStreamSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_cov","status":"in_progress","model":"` + coverageUpstreamResponsesModel + `"}}` + "\n\n" +
	`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_cov","status":"completed","model":"` + coverageUpstreamResponsesModel + `","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

const coverageResponsesJSON = `{"id":"resp_cov","object":"response","status":"completed","model":"` + coverageUpstreamResponsesModel +
	`","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`

const coverageChatStreamSSE = `data: {"id":"chatcmpl_cov","object":"chat.completion.chunk","created":1,"model":"` + coverageUpstreamChatModel +
	`","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl_cov","object":"chat.completion.chunk","created":1,"model":"` + coverageUpstreamChatModel +
	`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n" +
	"data: [DONE]\n\n"

const coverageChatJSON = `{"id":"chatcmpl_cov","object":"chat.completion","created":1,"model":"` + coverageUpstreamChatModel +
	`","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

const coverageMessagesStreamSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_cov","type":"message","role":"assistant","model":"` + coverageUpstreamClaudeModel +
	`","content":[],"usage":{"input_tokens":8,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// compact 的 body-signal 形态（compact_via_responses_enabled）：上游改走
// /responses + compaction_trigger，成功聚合回一次性 JSON，失败则以 response.failed
// 信封宣告——那依然是一份上游信封，必须记下它自报的模型。
const coverageCompactStreamSSE = "event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_compact","status":"completed","model":"` + coverageUpstreamResponsesModel +
	`","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

const coverageCompactFailedSSE = "event: response.failed\n" +
	`data: {"type":"response.failed","response":{"id":"resp_compact","status":"failed","status_code":400,"model":"` + coverageUpstreamResponsesModel +
	`","error":{"code":"context_length_exceeded","message":"compact input too large"}}}` + "\n\n"

// Antigravity OAuth 的 Gemini 原始回包：真正的上游自报模型在 modelVersion 里，
// 但适配器会把整条流改写成 Responses 信封并丢掉它（见 antigravity_responses.go）。
const coverageAntigravityGeminiSSE = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.6-flash-real","usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}` + "\n\n"

const coverageAntigravityModel = "gemini-3.6-flash-low"

const coverageMessagesJSON = `{"id":"msg_cov","type":"message","role":"assistant","model":"` + coverageUpstreamClaudeModel +
	`","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":5}}`

// upstreamModelBranchCase 是一条分支：账号形态 + 入站端点 + 上游桩响应。
type upstreamModelBranchCase struct {
	name string
	// path/body 是下游请求。
	path string
	body string
	// contentType/payload 是桩上游的回包。
	contentType string
	payload     string
	// account 构造这一分支需要的号池；upstreamURL 是桩上游地址。
	account func(t *testing.T, store *auth.Store, upstreamURL string)
	// resin 为 true 时把官方 Codex 出口改指到桩上游（官方账号没有 BaseURL 可改）。
	resin bool
	// settings 调整这一分支依赖的运行期开关。
	settings func(RuntimeSettings) RuntimeSettings
	want     string
	// official 标记调度账号是官方 Codex OAuth 账号：只有它才记录窗口号。
	official bool
	// wantStatus 是这条分支期望的落库状态码与下游状态码；0 表示 200。
	// 上游用 response.failed 信封宣告失败的分支同样必须记下上游自报模型。
	wantStatus int
}

func upstreamModelCoverageCases() []upstreamModelBranchCase {
	relayAccount := func(models []string) func(*testing.T, *auth.Store, string) {
		return func(t *testing.T, store *auth.Store, upstreamURL string) {
			t.Helper()
			store.AddAccount(&auth.Account{
				DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstreamURL,
				APIKey: "coverage-relay", Models: models, PlanType: "api",
			})
		}
	}
	codexAccount := func(models []string) func(*testing.T, *auth.Store, string) {
		return func(t *testing.T, store *auth.Store, _ string) {
			t.Helper()
			store.AddAccount(&auth.Account{
				DBID: 1, AccessToken: "coverage-codex", PlanType: "pro", Models: models,
			})
		}
	}
	grokNativeAccount := func(protocol GrokProtocol, models []string) func(*testing.T, *auth.Store, string) {
		return func(t *testing.T, store *auth.Store, upstreamURL string) {
			t.Helper()
			baseURL := strings.TrimRight(upstreamURL, "/") + "/v1"
			account := &auth.Account{
				DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "coverage-grok", BaseURL: baseURL,
				Models: models, PlanType: "api",
			}
			account.SetGrokRoutingState(auth.GrokRoutingState{
				Models: []auth.GrokModelRoute{{ModelID: models[0], BaseURL: baseURL, APIBackend: protocol}},
				Capabilities: []auth.GrokProtocolCapability{{
					ModelID: models[0], Origin: baseURL, Protocol: protocol,
					Status: auth.GrokCapabilityOK, ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
				}},
			})
			store.AddAccount(account)
		}
	}
	claudeNativeAccount := func(models []string) func(*testing.T, *auth.Store, string) {
		return func(t *testing.T, store *auth.Store, upstreamURL string) {
			t.Helper()
			store.AddAccount(&auth.Account{
				DBID: 1, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey,
				AccessToken: "coverage-claude", ClaudeBaseURL: upstreamURL, Models: models,
				Status: auth.StatusReady,
			})
		}
	}
	compactViaResponses := func(current RuntimeSettings) RuntimeSettings {
		current.CompactViaResponses = true
		return current
	}
	// Antigravity OAuth：把 OAuth 出口改指到桩上游，账号只给 AccessToken +
	// ProjectID（不给 APIKey），auth kind 才会判定成 OAuth。
	antigravityOAuthAccount := func() func(*testing.T, *auth.Store, string) {
		return func(t *testing.T, store *auth.Store, upstreamURL string) {
			t.Helper()
			previous := antigravityOAuthEndpointBases
			antigravityOAuthEndpointBases = []string{upstreamURL}
			t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
			store.AddAccount(&auth.Account{
				DBID: 1, UpstreamType: auth.UpstreamAntigravity,
				AccessToken: "coverage-ag-token", RefreshToken: "coverage-ag-refresh",
				AntigravityProjectID: "coverage-project",
				Models:               []string{coverageAntigravityModel},
			})
		}
	}
	preflight := func(on bool) func(RuntimeSettings) RuntimeSettings {
		return func(current RuntimeSettings) RuntimeSettings {
			current.CodexPreflightSSEPassthrough = on
			return current
		}
	}

	return []upstreamModelBranchCase{
		{
			name: "responses/relay/stream/preflight-on", path: "/v1/responses",
			body:        `{"model":"gpt-5.5","input":"hi","stream":true}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: relayAccount([]string{"gpt-5.5"}), settings: preflight(true),
			want: coverageUpstreamResponsesModel,
		},
		{
			name: "responses/relay/stream/preflight-off", path: "/v1/responses",
			body:        `{"model":"gpt-5.5","input":"hi","stream":true}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: relayAccount([]string{"gpt-5.5"}), settings: preflight(false),
			want: coverageUpstreamResponsesModel,
		},
		{
			name: "responses/relay/non-stream", path: "/v1/responses",
			body:        `{"model":"gpt-5.5","input":"hi","stream":false}`,
			contentType: "application/json", payload: coverageResponsesJSON,
			account: relayAccount([]string{"gpt-5.5"}),
			want:    coverageUpstreamResponsesModel,
		},
		{
			name: "responses/codex/stream", path: "/v1/responses",
			body:        `{"model":"gpt-5.5","input":"hi","stream":true}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: codexAccount([]string{"gpt-5.5"}), resin: true, official: true,
			want: coverageUpstreamResponsesModel,
		},
		{
			name: "responses/codex/non-stream", path: "/v1/responses",
			body:        `{"model":"gpt-5.5","input":"hi","stream":false}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: codexAccount([]string{"gpt-5.5"}), resin: true, official: true,
			want: coverageUpstreamResponsesModel,
		},
		{
			name: "chat/codex/stream", path: "/v1/chat/completions",
			body:        `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: codexAccount([]string{"gpt-5.5"}), resin: true, official: true,
			want: coverageUpstreamResponsesModel,
		},
		{
			name: "chat/relay/stream", path: "/v1/chat/completions",
			body:        `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: relayAccount([]string{"gpt-5.5"}),
			want:    coverageUpstreamResponsesModel,
		},
		{
			name: "chat/relay/non-stream", path: "/v1/chat/completions",
			body:        `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: relayAccount([]string{"gpt-5.5"}),
			want:    coverageUpstreamResponsesModel,
		},
		{
			name: "chat/grok-native/stream", path: "/v1/chat/completions",
			body:        `{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			contentType: "text/event-stream", payload: coverageChatStreamSSE,
			account: grokNativeAccount(GrokProtocolChatCompletions, []string{"grok-4.5"}),
			want:    coverageUpstreamChatModel,
		},
		{
			name: "chat/grok-native/non-stream", path: "/v1/chat/completions",
			body:        `{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			contentType: "application/json", payload: coverageChatJSON,
			account: grokNativeAccount(GrokProtocolChatCompletions, []string{"grok-4.5"}),
			want:    coverageUpstreamChatModel,
		},
		{
			name: "responses/grok-native/stream", path: "/v1/responses",
			body:        `{"model":"grok-4.5","input":"hi","stream":true}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: grokNativeAccount(GrokProtocolResponses, []string{"grok-4.5"}),
			want:    coverageUpstreamResponsesModel,
		},
		{
			name: "messages/codex/stream", path: "/v1/messages",
			body:        `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: codexAccount([]string{"gpt-5.5"}), resin: true, official: true,
			want: coverageUpstreamResponsesModel,
		},
		{
			name: "messages/relay/non-stream", path: "/v1/messages",
			body:        `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":false}`,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			account: relayAccount([]string{"gpt-5.5"}),
			want:    coverageUpstreamResponsesModel,
		},
		{
			name: "compact/relay/non-stream", path: "/v1/responses/compact",
			body:        `{"model":"gpt-5.5","input":"hi"}`,
			contentType: "application/json", payload: coverageResponsesJSON,
			account: relayAccount([]string{"gpt-5.5"}),
			want:    coverageUpstreamResponsesModel,
		},
		{
			name: "compact/codex/via-responses/completed", path: "/v1/responses/compact",
			body:        `{"model":"gpt-5.5","input":"hi"}`,
			contentType: "text/event-stream", payload: coverageCompactStreamSSE,
			account: codexAccount([]string{"gpt-5.5"}), resin: true, official: true,
			settings: compactViaResponses,
			want:     coverageUpstreamResponsesModel,
		},
		{
			name: "compact/codex/via-responses/failed-envelope", path: "/v1/responses/compact",
			body:        `{"model":"gpt-5.5","input":"hi"}`,
			contentType: "text/event-stream", payload: coverageCompactFailedSSE,
			account: codexAccount([]string{"gpt-5.5"}), resin: true, official: true,
			settings:   compactViaResponses,
			want:       coverageUpstreamResponsesModel,
			wantStatus: http.StatusBadRequest,
		},
		{
			// 适配器合成的信封里 model 写的是网关这次请求用的模型，不是上游声明。
			// 记下来就等于「从请求反推」，这一列明令禁止——必须留空。
			name: "responses/antigravity-oauth/stream", path: "/v1/responses",
			body:        `{"model":"` + coverageAntigravityModel + `","input":"hi","stream":true}`,
			contentType: "text/event-stream", payload: coverageAntigravityGeminiSSE,
			account: antigravityOAuthAccount(),
			want:    "",
		},
		{
			name: "chat/antigravity-oauth/stream", path: "/v1/chat/completions",
			body:        `{"model":"` + coverageAntigravityModel + `","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			contentType: "text/event-stream", payload: coverageAntigravityGeminiSSE,
			account: antigravityOAuthAccount(),
			want:    "",
		},
		{
			name: "messages/claude-native/stream", path: "/v1/messages",
			body:        `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			contentType: "text/event-stream", payload: coverageMessagesStreamSSE,
			account: claudeNativeAccount([]string{"claude-sonnet-4-5"}),
			want:    coverageUpstreamClaudeModel,
		},
		{
			name: "messages/claude-native/non-stream", path: "/v1/messages",
			body:        `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":false}`,
			contentType: "application/json", payload: coverageMessagesJSON,
			account: claudeNativeAccount([]string{"claude-sonnet-4-5"}),
			want:    coverageUpstreamClaudeModel,
		},
	}
}

func TestUsageLogRecordsUpstreamModelOnEveryBranch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range upstreamModelCoverageCases() {
		t.Run(tc.name, func(t *testing.T) {
			logs := runUpstreamModelBranch(t, tc)
			if len(logs) == 0 {
				t.Fatal("没有产生任何用量日志行，分支根本没跑到")
			}
			wantStatus := tc.wantStatus
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			for _, entry := range logs {
				if entry.StatusCode != wantStatus {
					t.Fatalf("落库状态 = %d, want %d（%s）", entry.StatusCode, wantStatus, entry.ErrorMessage)
				}
				if entry.UpstreamResponseModel != tc.want {
					t.Fatalf("UpstreamResponseModel = %q, want %q（endpoint=%s stream=%t）",
						entry.UpstreamResponseModel, tc.want, entry.Endpoint, entry.Stream)
				}
				wantWindow := ""
				if tc.official {
					wantWindow = coverageWindowNumber
				}
				if entry.WindowNumber != wantWindow {
					t.Fatalf("WindowNumber = %q, want %q（官方 Codex 账号=%t）",
						entry.WindowNumber, wantWindow, tc.official)
				}
			}
		})
	}
}

func runUpstreamModelBranch(t *testing.T, tc upstreamModelBranchCase) []*database.UsageLog {
	t.Helper()
	previousSettings := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	if tc.settings != nil {
		settings = tc.settings(settings)
	}
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", tc.contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, tc.payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	if tc.resin {
		previousResin := resinCfg.Load()
		t.Cleanup(func() { resinCfg.Store(previousResin) })
		SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "upstream-model-coverage"})
	}

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "upstream-model-coverage.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetUsageLogConfig(database.UsageLogModeFull, 1, 1)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	tc.account(t, store, upstream.URL)

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)

	request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
	request.Header.Set("Content-Type", "application/json")
	// 窗口号只对官方 Codex 账号记录：每条分支都带上同一个 window-id，
	// 由断言按账号类型区分「记 / 不记」。
	request.Header.Set(codexWindowIDHeader, coverageWindowID)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	wantDownstream := tc.wantStatus
	if wantDownstream == 0 {
		wantDownstream = http.StatusOK
	}
	if recorder.Code != wantDownstream {
		t.Fatalf("下游状态 = %d, want %d; body=%s", recorder.Code, wantDownstream, recorder.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		db.FlushUsageLogs()
		logs, listErr := db.ListRecentUsageLogs(context.Background(), 10)
		if listErr != nil {
			t.Fatalf("ListRecentUsageLogs: %v", listErr)
		}
		if len(logs) > 0 {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待用量日志落库超时；下游 body=%s", recorder.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
