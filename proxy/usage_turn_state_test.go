package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 上游真实 turn-state 的长度是账号级的「降智桶」标记（线上实测：健康号 292 字符，
// 其余九个号 312 字符）。这些用例锁住采集链路的每一段：入站分类写进上下文、上游
// 下发时按「本次尝试」记长度、落库时只对官方 Codex 账号生效。
const usageTurnStateProbeToken = "TS-0123456789abcdef"

func newUsageTurnStateTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	return c
}

func usageTurnStateInputFor(t *testing.T, h *Handler, c *gin.Context, accountID int64) *database.UsageLogInput {
	t.Helper()
	input := &database.UsageLogInput{AccountID: accountID, Endpoint: "/v1/responses", StatusCode: 200}
	h.populateUsageTurnState(c, input)
	return input
}

// (b)(c) 入站回带分类：本会话替身换回真实值记 substitute（没被剥离），外来 token
// 记 cross 并标记已剥离。这两类正是「降智账号是否与外来 turn-state 相关」的读数。
func TestPopulateUsageTurnStateRecordsInboundEchoClass(t *testing.T) {
	resetSessionGuardStatsForTest()
	minter := &auth.Account{DBID: 401, AccessToken: "tok"}
	other := &auth.Account{DBID: 402, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, minter, other)

	t.Run("substitute restored", func(t *testing.T) {
		key := "usage-ts-substitute::api-key:9"
		t.Cleanup(func() { turnStateVault.Delete(key) })
		substitute := issueCodexTurnStateSubstitute(key, minter, usageTurnStateProbeToken)
		if substitute == "" {
			t.Fatal("issueCodexTurnStateSubstitute returned an empty substitute")
		}
		c := newUsageTurnStateTestContext()
		headers := http.Header{}
		headers.Set(codexTurnStateHeader, substitute)
		if _, _, stripped := h.applyCodexTurnStateEchoPolicy(c, key, minter, headers, []byte(`{}`)); stripped {
			t.Fatal("a restored substitute must not be stripped")
		}
		input := usageTurnStateInputFor(t, h, c, minter.ID())
		if input.TurnStateEcho != "substitute" || input.TurnStateStripped {
			t.Fatalf("echo = %q stripped = %v, want substitute/false", input.TurnStateEcho, input.TurnStateStripped)
		}
	})

	t.Run("cross stripped", func(t *testing.T) {
		key := "usage-ts-cross::api-key:9"
		t.Cleanup(func() { turnStateVault.Delete(key) })
		substitute := issueCodexTurnStateSubstitute(key, minter, usageTurnStateProbeToken)
		c := newUsageTurnStateTestContext()
		headers := http.Header{}
		headers.Set(codexTurnStateHeader, substitute)
		// 换号之后客户端仍回带上一个账号的替身：cross + 剥离。
		if _, class, stripped := h.applyCodexTurnStateEchoPolicy(c, key, other, headers, []byte(`{}`)); class != turnStateEchoCross || !stripped {
			t.Fatalf("class = %s stripped = %v, want cross/true", class, stripped)
		}
		input := usageTurnStateInputFor(t, h, c, other.ID())
		if input.TurnStateEcho != "cross" || !input.TurnStateStripped {
			t.Fatalf("echo = %q stripped = %v, want cross/true", input.TurnStateEcho, input.TurnStateStripped)
		}
	})

	t.Run("no inbound token", func(t *testing.T) {
		c := newUsageTurnStateTestContext()
		if _, class, _ := h.applyCodexTurnStateEchoPolicy(c, "usage-ts-none::api-key:9", minter, http.Header{}, []byte(`{}`)); class != turnStateEchoNone {
			t.Fatalf("class = %s, want none", class)
		}
		input := usageTurnStateInputFor(t, h, c, minter.ID())
		if input.TurnStateEcho != "none" || input.TurnStateStripped {
			t.Fatalf("echo = %q stripped = %v, want none/false", input.TurnStateEcho, input.TurnStateStripped)
		}
	})
}

// failover 不覆盖首个判定：客户端带了什么是这一轮的事实，换号不会改变它。
func TestPopulateUsageTurnStateKeepsFirstEchoDecision(t *testing.T) {
	resetSessionGuardStatsForTest()
	disableTurnStateVault(t)
	minter := &auth.Account{DBID: 411, AccessToken: "tok"}
	other := &auth.Account{DBID: 412, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, minter, other)
	key := "usage-ts-first::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(key) })
	noteCodexTurnStateProvenance(key, minter)

	c := newUsageTurnStateTestContext()
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "blob")
	h.applyCodexTurnStateEchoPolicy(c, key, minter, headers, []byte(`{}`))
	headers.Set(codexTurnStateHeader, "blob")
	h.applyCodexTurnStateEchoPolicy(c, key, other, headers, []byte(`{}`))

	input := usageTurnStateInputFor(t, h, c, other.ID())
	if input.TurnStateEcho != "same" || input.TurnStateStripped {
		t.Fatalf("echo = %q stripped = %v, want the first attempt's same/false", input.TurnStateEcho, input.TurnStateStripped)
	}
}

// (e) 拿到上游响应之前就失败的尝试记 NULL：没检查过就不能说「上游没给」。
func TestPopulateUsageTurnStateLeavesLengthNullWithoutUpstreamResponse(t *testing.T) {
	account := &auth.Account{DBID: 421, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, account)
	c := newUsageTurnStateTestContext()
	beginUsageTurnStateAttempt(c)

	input := usageTurnStateInputFor(t, h, c, account.ID())
	if input.TurnStateLength != nil {
		t.Fatalf("TurnStateLength = %v, want NULL", *input.TurnStateLength)
	}
}

func TestMarkUsageTurnStateCheckedRecordsLengthAndExplicitZero(t *testing.T) {
	account := &auth.Account{DBID: 431, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, account)

	c := newUsageTurnStateTestContext()
	beginUsageTurnStateAttempt(c)
	markUsageTurnStateChecked(c, len(usageTurnStateProbeToken))
	input := usageTurnStateInputFor(t, h, c, account.ID())
	if input.TurnStateLength == nil || *input.TurnStateLength != len(usageTurnStateProbeToken) {
		t.Fatalf("TurnStateLength = %v, want %d", input.TurnStateLength, len(usageTurnStateProbeToken))
	}

	// 检查过但上游没给 → 0，与 NULL 区分。
	empty := newUsageTurnStateTestContext()
	beginUsageTurnStateAttempt(empty)
	markUsageTurnStateChecked(empty, 0)
	input = usageTurnStateInputFor(t, h, empty, account.ID())
	if input.TurnStateLength == nil || *input.TurnStateLength != 0 {
		t.Fatalf("TurnStateLength = %v, want an explicit 0", input.TurnStateLength)
	}

	// 同一次尝试里响应头与 metadata 事件两个载体都会汇报；先记到的真实长度不能被
	// 后来的「检查过但没有」覆盖回 0。
	both := newUsageTurnStateTestContext()
	beginUsageTurnStateAttempt(both)
	markUsageTurnStateChecked(both, 292)
	markUsageTurnStateChecked(both, 0)
	input = usageTurnStateInputFor(t, h, both, account.ID())
	if input.TurnStateLength == nil || *input.TurnStateLength != 292 {
		t.Fatalf("TurnStateLength = %v, want 292 kept", input.TurnStateLength)
	}
}

// (f) 旧流的迟到事件不能写进新尝试的槽位：换号之后旧账号的 292 会把新账号的
// 「没拿到」盖掉，正好把要排查的降智账号显示成健康的。
func TestBeginUsageTurnStateAttemptIsolatesLateEvents(t *testing.T) {
	account := &auth.Account{DBID: 441, AccessToken: "tok"}
	h := newSessionGuardTestHandler(t, account)
	c := newUsageTurnStateTestContext()

	firstAttempt := beginUsageTurnStateAttempt(c)
	beginUsageTurnStateAttempt(c)
	// 第一次尝试的流仍在排水，事件晚到：写进它自己捕获的槽位，对当前尝试不可见。
	firstAttempt.mark(292)

	input := usageTurnStateInputFor(t, h, c, account.ID())
	if input.TurnStateLength != nil {
		t.Fatalf("TurnStateLength = %v, want NULL for the new attempt", *input.TurnStateLength)
	}

	markUsageTurnStateChecked(c, 312)
	input = usageTurnStateInputFor(t, h, c, account.ID())
	if input.TurnStateLength == nil || *input.TurnStateLength != 312 {
		t.Fatalf("TurnStateLength = %v, want the current attempt's 312", input.TurnStateLength)
	}
}

// (d) 中转 / 非官方账号一律留空：它们服务的客户端可以是任何东西，回带的 blob
// 不具备同一套语义，记下来只会让按账号读数的页面把互不相干的值并排显示。
func TestPopulateUsageTurnStateSkipsNonOfficialAccounts(t *testing.T) {
	relay := &auth.Account{DBID: 451, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "k", PlanType: "api"}
	h := newSessionGuardTestHandler(t, relay)
	c := newUsageTurnStateTestContext()
	beginUsageTurnStateAttempt(c)
	markUsageTurnStateChecked(c, 292)
	noteUsageTurnStateEcho(c, usageTurnStateEchoCross, true)

	input := usageTurnStateInputFor(t, h, c, relay.ID())
	if input.TurnStateLength != nil || input.TurnStateEcho != "" || input.TurnStateStripped {
		t.Fatalf("relay row = %v/%q/%v, want NULL/''/false", input.TurnStateLength, input.TurnStateEcho, input.TurnStateStripped)
	}
}

// (a)(d)(e) 端到端：真实 handler + 桩上游，按账号形态断言落库的三列。
func TestUsageTurnStateColumnsEndToEnd(t *testing.T) {
	cases := []struct {
		name         string
		official     bool
		upstreamCode int
		token        string
		payload      string
		contentType  string
		wantStatus   int
		wantLength   *int
		wantEcho     string
	}{
		{
			name: "official stream records the upstream token length", official: true,
			upstreamCode: http.StatusOK, token: usageTurnStateProbeToken,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			wantLength: intPtr(len(usageTurnStateProbeToken)), wantEcho: "none",
		},
		{
			name: "official stream without an upstream token records an explicit zero", official: true,
			upstreamCode: http.StatusOK, token: "",
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			wantLength: intPtr(0), wantEcho: "none",
		},
		{
			name: "relay account records nothing", official: false,
			upstreamCode: http.StatusOK, token: usageTurnStateProbeToken,
			contentType: "text/event-stream", payload: coverageResponsesStreamSSE,
			wantLength: nil, wantEcho: "",
		},
		{
			name: "attempt that failed before the upstream response keeps NULL", official: true,
			upstreamCode: http.StatusBadRequest, token: "",
			contentType: "application/json",
			payload:     `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too large"}}`,
			wantStatus:  http.StatusBadRequest, wantLength: nil, wantEcho: "none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previousSettings := CurrentRuntimeSettings()
			settings := DefaultRuntimeSettings()
			settings.CodexForceWebsocket = false
			ApplyRuntimeSettings(settings)
			t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.token != "" {
					w.Header().Set(codexTurnStateHeader, tc.token)
				}
				w.Header().Set("Content-Type", tc.contentType)
				code := tc.upstreamCode
				if code == 0 {
					code = http.StatusOK
				}
				w.WriteHeader(code)
				_, _ = io.WriteString(w, tc.payload)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}))
			t.Cleanup(upstream.Close)

			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "usage-turn-state.db"))
			if err != nil {
				t.Fatalf("database.New: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			db.SetUsageLogConfig(database.UsageLogModeFull, 1, 1)

			store := auth.NewStore(nil, nil, &database.SystemSettings{
				MaxConcurrency: 2, TestConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0,
			})
			t.Cleanup(store.Stop)
			if tc.official {
				previousResin := resinCfg.Load()
				t.Cleanup(func() { resinCfg.Store(previousResin) })
				SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "usage-turn-state"})
				store.AddAccount(&auth.Account{DBID: 1, AccessToken: "ts-codex", PlanType: "pro", Models: []string{"gpt-5.5"}})
			} else {
				store.AddAccount(&auth.Account{
					DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL,
					APIKey: "ts-relay", Models: []string{"gpt-5.5"}, PlanType: "api",
				})
			}

			handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
			router := gin.New()
			handler.RegisterRoutes(router)

			request := httptest.NewRequest(http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"gpt-5.5","input":"hi","stream":true}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			logs := waitForUsageTurnStateLogs(t, db, recorder)
			row := logs[0]
			if tc.wantLength == nil {
				if row.TurnStateLength != nil {
					t.Fatalf("TurnStateLength = %d, want NULL", *row.TurnStateLength)
				}
			} else if row.TurnStateLength == nil || *row.TurnStateLength != *tc.wantLength {
				t.Fatalf("TurnStateLength = %v, want %d", row.TurnStateLength, *tc.wantLength)
			}
			if row.TurnStateEcho != tc.wantEcho {
				t.Fatalf("TurnStateEcho = %q, want %q", row.TurnStateEcho, tc.wantEcho)
			}
		})
	}
}

func intPtr(value int) *int { return &value }

func waitForUsageTurnStateLogs(t *testing.T, db *database.DB, recorder *httptest.ResponseRecorder) []*database.UsageLog {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		db.FlushUsageLogs()
		logs, err := db.ListRecentUsageLogs(context.Background(), 10)
		if err != nil {
			t.Fatalf("ListRecentUsageLogs: %v", err)
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

// TestUsageTurnStateWiringPresent 钉住采集点：这三件事只有请求侧拿得到，任何一处
// 被删掉或漏在新分支上，都只会表现为该路径的 turn-state 永久缺失——落库链路本身
// 不会报错，普通用例也测不出来。
func TestUsageTurnStateWiringPresent(t *testing.T) {
	sources := map[string][]byte{}
	for _, name := range []string{"handler.go", "responses_ws.go", "codex_turn_state.go", "session_guards.go"} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = content
	}
	count := func(name, pattern string) int {
		return len(regexp.MustCompile(pattern).FindAll(sources[name], -1))
	}
	// 每次尝试换一个新槽位：HTTP 的 Responses 循环与 WS 的轮次循环各一处。
	if got := count("handler.go", `\n\t\tturnStateUsageSlot := beginUsageTurnStateAttempt\(c\)`); got != 1 {
		t.Fatalf("handler.go beginUsageTurnStateAttempt sites = %d, want 1", got)
	}
	if got := count("responses_ws.go", `\n\t\tbeginUsageTurnStateAttempt\(c\)`); got != 1 {
		t.Fatalf("responses_ws.go beginUsageTurnStateAttempt sites = %d, want 1", got)
	}
	// WS 的一条 gin.Context 服务多轮，每轮必须清一次回带分类。
	if got := count("responses_ws.go", `beginUsageTurnStateTurn\(c\)`); got != 1 {
		t.Fatalf("responses_ws.go beginUsageTurnStateTurn sites = %d, want 1", got)
	}
	// 落库填充链里只补一次，且紧跟窗口号（同一类「只有请求侧知道」的字段）。
	if got := count("handler.go", `h\.populateUsageWindowNumber\(c, input\)\n\th\.populateUsageTurnState\(c, input\)`); got != 1 {
		t.Fatalf("handler.go populateUsageTurnState wiring = %d, want 1 right after the window number", got)
	}
	// 上游响应头的两个下发点都要记真实长度（替身改写之前）。
	if got := count("codex_turn_state.go", `markUsageTurnStateChecked\(c, len\(token\)\)`); got != 2 {
		t.Fatalf("codex_turn_state.go markUsageTurnStateChecked sites = %d, want 2", got)
	}
	// 入站分类的两条返回路径都要记（没带 token 的 none 也是一条事实）。
	if got := count("session_guards.go", `noteUsageTurnStateEcho\(c, `); got != 2 {
		t.Fatalf("session_guards.go noteUsageTurnStateEcho sites = %d, want 2", got)
	}
}
