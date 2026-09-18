package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// 单元：计时口径本身
// ---------------------------------------------------------------------------

func TestUpstreamFirstResponseTimingNormalCommitAndRetry(t *testing.T) {
	start := time.Now()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	failed := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start}
	failed.observe(gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(100*time.Millisecond))
	if c.Writer.Written() {
		t.Fatal("observing metadata must not commit HTTP 200")
	}
	if len(recorder.Header()) != 0 {
		t.Fatalf("observing metadata staged downstream headers: %v", recorder.Header())
	}

	winner := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start.Add(2 * time.Second)}
	for _, event := range []string{"response.created", "response.in_progress", "response.failed", "response.completed", "error", "ping", "keepalive", "heartbeat"} {
		winner.observe(gjson.Parse(`{"type":"`+event+`"}`), start.Add(2100*time.Millisecond))
	}
	if winner.recorded {
		t.Fatal("a lifecycle, terminal or heartbeat event was recorded as the first response")
	}
	winner.observe(gjson.Parse(`{"type":"response.metadata"}`), start.Add(3*time.Second))
	winner.observe(gjson.Parse(`{"type":"response.output_text.delta","delta":"hello"}`), start.Add(10*time.Second))

	relayUpstreamFirstResponseHeaders(c, &winner)
	if c.Writer.Written() {
		t.Fatal("staging timing must not flush or write a body")
	}
	c.String(http.StatusOK, "hello")
	result := recorder.Result()
	if got := result.Header.Get(upstreamFirstResponseHeader); got != "3000" {
		t.Fatalf("request timing = %q, want 3000 (includes the earlier attempt and the retry wait)", got)
	}
	if got := result.Header.Get(upstreamAttemptFirstResponseHeader); got != "1000" {
		t.Fatalf("attempt timing = %q, want 1000 (the winning attempt only)", got)
	}
	if recorder.Body.String() != "hello" {
		t.Fatalf("body = %q, want hello", recorder.Body.String())
	}
	// 已提交的响应头不能再被后来的事件或被放弃的 attempt 改写。
	relayUpstreamFirstResponseHeaders(c, &failed)
	if got := result.Header.Get(upstreamFirstResponseHeader); got != "3000" {
		t.Fatalf("committed request timing changed to %q", got)
	}
}

func TestUpstreamFirstResponseDisabledOrCommittedFallsBack(t *testing.T) {
	start := time.Now()
	disabled := upstreamFirstResponseTiming{requestStart: start, attemptStart: start}
	disabled.observe(gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(time.Second))
	if disabled.recorded {
		t.Fatal("the disabled switch still recorded a first response")
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Writer.WriteHeaderNow() // 已有心跳提前提交了响应头。
	enabled := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start}
	enabled.observe(gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(time.Second))
	relayUpstreamFirstResponseHeaders(c, &enabled)
	if got := recorder.Result().Header.Get(upstreamTimingHeader); got != "" {
		t.Fatalf("committed response still reported timing: %q", got)
	}
}

func TestUpstreamFirstResponseTimingRecordsLooseFirstTokenOnce(t *testing.T) {
	start := time.Now()
	timing := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start}
	ignored := []string{
		`{"type":"response.created"}`,
		`{"type":"response.in_progress"}`,
		`{"type":"response.completed"}`,
		`{"type":"response.failed"}`,
		`{"type":"response.incomplete"}`,
		`{"type":"response.cancelled"}`,
		`{"type":"error"}`,
		`{"type":"response.error"}`,
		`{"type":"ping"}`,
		`{"type":"keepalive"}`,
		`{"type":"heartbeat"}`,
		`{"delta":"no type at all"}`,
	}
	for _, event := range ignored {
		timing.observe(gjson.Parse(event), start.Add(time.Second))
		if timing.recorded {
			t.Fatalf("event %s was recorded as the first response", event)
		}
	}

	timing.observe(gjson.Parse(`{"type":"response.output_text.delta","delta":"hi"}`), start.Add(2*time.Second))
	if !timing.recorded || timing.requestMS != 2000 || timing.attemptMS != 2000 {
		t.Fatalf("timing = %+v, want recorded at 2000/2000", timing)
	}

	timing.observe(gjson.Parse(`{"type":"response.output_text.delta","delta":"later"}`), start.Add(9*time.Second))
	if timing.requestMS != 2000 {
		t.Fatalf("a later content event rewrote the recorded timing: %d", timing.requestMS)
	}
}

func TestUpstreamFirstResponseAttemptNeverExceedsRequest(t *testing.T) {
	now := time.Now()
	// 换号后 attempt 时钟可能比请求级时钟更早；attempt 值必须被夹到请求值以内，
	// 否则「快的替身」会看起来比整条请求还快。
	clamped := upstreamFirstResponseTiming{enabled: true, requestStart: now.Add(500 * time.Millisecond), attemptStart: now}
	clamped.observe(gjson.Parse(`{"type":"codex.rate_limits"}`), now.Add(time.Second))
	if clamped.requestMS != 500 || clamped.attemptMS != 500 {
		t.Fatalf("timing = %+v, want the attempt clamped to the request value 500", clamped)
	}

	future := upstreamFirstResponseTiming{enabled: true, requestStart: now.Add(2 * time.Second), attemptStart: now.Add(2 * time.Second)}
	future.observe(gjson.Parse(`{"type":"codex.rate_limits"}`), now)
	if future.requestMS != 0 || future.attemptMS != 0 {
		t.Fatalf("timing = %+v, want 0/0 for a non-positive elapsed duration", future)
	}
}

func TestClearUpstreamFirstResponseHeadersRemovesEveryTimingHeader(t *testing.T) {
	headers := make(http.Header)
	headers.Set(upstreamTimingHeader, "v1-loose")
	headers.Set(upstreamFirstResponseHeader, "12")
	headers.Set(upstreamAttemptFirstResponseHeader, "3")
	headers.Set("X-Unrelated", "keep")
	clearUpstreamFirstResponseHeaders(headers)
	for _, name := range []string{upstreamTimingHeader, upstreamFirstResponseHeader, upstreamAttemptFirstResponseHeader} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("%s survived the clear: %q", name, got)
		}
	}
	if got := headers.Get("X-Unrelated"); got != "keep" {
		t.Fatalf("unrelated header = %q, want keep", got)
	}
}

// 只有本网关自己量出来的数字才能下发。中转分支根本没有计时结构，传 nil；
// 上一 attempt 暂存在 writer 上的值也必须被抹掉。
func TestRelayUpstreamFirstResponseHeadersNeedsThisGatewaysOwnMeasurement(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		timing *upstreamFirstResponseTiming
	}{
		{name: "no timing at all"},
		{name: "enabled but never recorded", timing: &upstreamFirstResponseTiming{enabled: true, requestStart: now, attemptStart: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Header(upstreamTimingHeader, "v1-loose")
			c.Header(upstreamFirstResponseHeader, "999")
			c.Header(upstreamAttemptFirstResponseHeader, "999")

			relayUpstreamFirstResponseHeaders(c, tc.timing)
			for _, name := range []string{upstreamTimingHeader, upstreamFirstResponseHeader, upstreamAttemptFirstResponseHeader} {
				if got := c.Writer.Header().Get(name); got != "" {
					t.Fatalf("published %s = %q without a recorded measurement", name, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 端到端：真实 handler + 桩上游
// ---------------------------------------------------------------------------

const firstResponseTimingCodexModel = "gpt-5.5"
const firstResponseTimingRelayModel = "gpt-4.1-direct"
const firstResponseTimingText = "first-response-hello"

// 桩上游自报的计时头：上游若自己就是一台开了本开关的 codex2api，这些值绝不能
// 被当成本网关的测量下发。
const firstResponseTimingForgedMS = "999999"

// firstResponseCommitRecorder 记录响应「第一次提交」的时刻相对于桩上游放行
// 首个内容事件的先后。提前提交 200 的回归会在放行之前就置位。
type firstResponseCommitRecorder struct {
	*httptest.ResponseRecorder
	mu             sync.Mutex
	committed      bool
	afterFirstData bool
	firstDataSent  *atomic.Bool
}

func newFirstResponseCommitRecorder(firstDataSent *atomic.Bool) *firstResponseCommitRecorder {
	return &firstResponseCommitRecorder{ResponseRecorder: httptest.NewRecorder(), firstDataSent: firstDataSent}
}

func (r *firstResponseCommitRecorder) note() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.committed {
		return
	}
	r.committed = true
	r.afterFirstData = r.firstDataSent == nil || r.firstDataSent.Load()
}

func (r *firstResponseCommitRecorder) WriteHeader(code int) {
	r.note()
	r.ResponseRecorder.WriteHeader(code)
}

func (r *firstResponseCommitRecorder) Write(payload []byte) (int, error) {
	r.note()
	return r.ResponseRecorder.Write(payload)
}

func (r *firstResponseCommitRecorder) WriteString(payload string) (int, error) {
	r.note()
	return r.ResponseRecorder.WriteString(payload)
}

func (r *firstResponseCommitRecorder) Flush() {
	r.note()
	r.ResponseRecorder.Flush()
}

func (r *firstResponseCommitRecorder) commitState() (committed bool, afterFirstData bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.committed, r.afterFirstData
}

// firstResponseUpstreamStub 是一个可编排节奏的 Responses 桩上游：先发前置元数据，
// 可选地卡住，再发首个内容事件与终态。
type firstResponseUpstreamStub struct {
	calls      atomic.Int32
	failFirst  bool
	firstDelay time.Duration
	// forgeTiming 让桩上游自报三个计时头，模拟「上游是另一台 codex2api」。
	forgeTiming bool
	// preflight 在前置元数据冲刷后关闭；release 关闭后才发首个内容事件。
	preflight   chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

// releaseGate 放行首个内容事件。测试失败路径也必须调用它，否则卡住的桩上游
// 会让 httptest.Server 的收尾一直等下去，把断言失败变成整包超时。
func (s *firstResponseUpstreamStub) releaseGate() {
	if s.release == nil {
		return
	}
	s.releaseOnce.Do(func() { close(s.release) })
}

func (s *firstResponseUpstreamStub) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "text/event-stream")
	if s.forgeTiming {
		w.Header().Set(upstreamTimingHeader, "v1-loose")
		w.Header().Set(upstreamFirstResponseHeader, firstResponseTimingForgedMS)
		w.Header().Set(upstreamAttemptFirstResponseHeader, firstResponseTimingForgedMS)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(frame string) {
		_, _ = io.WriteString(w, "data: "+frame+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	if s.calls.Add(1) == 1 && s.failFirst {
		time.Sleep(s.firstDelay)
		write(`{"type":"response.created","response":{"id":"resp_timing_failed"}}`)
		write(`{"type":"codex.rate_limits","plan_type":"plus"}`)
		write(`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"server_error","message":"temporary upstream failure"}}}`)
		return
	}

	write(`{"type":"response.created","response":{"id":"resp_timing"}}`)
	write(`{"type":"codex.rate_limits","plan_type":"plus"}`)
	if s.release != nil {
		s.once.Do(func() { close(s.preflight) })
		<-s.release
	}
	write(`{"type":"response.output_text.delta","delta":"` + firstResponseTimingText + `"}`)
	write(`{"type":"response.completed","response":{"id":"resp_timing","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + firstResponseTimingText + `"}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
}

type firstResponseTimingSetup struct {
	enabled          bool
	policy           database.ContinuousRetryPolicy
	relay            bool
	continueThinking bool
}

// newFirstResponseTimingHandler 装配「运行期开关 + 号池 + 桩上游出口」。relay 为
// true 时使用中转账号（不参与本计时契约），否则走官方 Codex OAuth 出口。
func newFirstResponseTimingHandler(t *testing.T, upstreamURL string, setup firstResponseTimingSetup) (*Handler, string) {
	t.Helper()
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	next := DefaultRuntimeSettings()
	next.CodexForceWebsocket = false
	next.CodexWSSilentRetry = false
	next.CodexWSSilentRetries = 0
	next.CodexPreflightSSEPassthrough = setup.enabled
	next.ContinuousRetryPolicy = setup.policy
	next.CodexContinueThinking = setup.continueThinking
	ApplyRuntimeSettings(next)

	if setup.relay {
		store := newOpenAIResponsesRelayStore(upstreamURL)
		t.Cleanup(store.Stop)
		return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil), firstResponseTimingRelayModel
	}

	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	SetResinConfig(&ResinConfig{BaseURL: upstreamURL, PlatformName: "first-response-timing"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, TestModel: firstResponseTimingCodexModel,
		MaxRetries: 1, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.SetRetryIntervalMS(0)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "first-response-token", PlanType: "pro", AccountID: "first-response-account"})
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil), firstResponseTimingCodexModel
}

func newFirstResponseTimingContext(model string, stream bool, writer http.ResponseWriter) *gin.Context {
	ctx, _ := gin.CreateTestContext(writer)
	body := `{"model":"` + model + `","input":"hello","stream":` + strconv.FormatBool(stream) + `}`
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	return ctx
}

func parseFirstResponseTimingHeaders(t *testing.T, headers http.Header) (requestMS int64, attemptMS int64) {
	t.Helper()
	if got := headers.Get(upstreamTimingHeader); got != "v1-loose" {
		t.Fatalf("timing version = %q, want v1-loose", got)
	}
	requestMS, err := strconv.ParseInt(headers.Get(upstreamFirstResponseHeader), 10, 64)
	if err != nil {
		t.Fatalf("request timing header %q is not an integer: %v", headers.Get(upstreamFirstResponseHeader), err)
	}
	attemptMS, err = strconv.ParseInt(headers.Get(upstreamAttemptFirstResponseHeader), 10, 64)
	if err != nil {
		t.Fatalf("attempt timing header %q is not an integer: %v", headers.Get(upstreamAttemptFirstResponseHeader), err)
	}
	if attemptMS < 0 || requestMS < attemptMS {
		t.Fatalf("timing ordering broken: request=%d attempt=%d", requestMS, attemptMS)
	}
	if requestMS > time.Minute.Milliseconds() {
		t.Fatalf("request timing = %d ms, implausible for a local stub upstream", requestMS)
	}
	return requestMS, attemptMS
}

func assertNoFirstResponseTimingHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	for _, name := range []string{upstreamTimingHeader, upstreamFirstResponseHeader, upstreamAttemptFirstResponseHeader} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("unexpected %s = %q", name, got)
		}
	}
}

func TestUpstreamFirstResponseHeadersAtNormalCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buffered := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	cases := []struct {
		name        string
		setup       firstResponseTimingSetup
		nonStream   bool
		forgeTiming bool
		wantHeaders bool
	}{
		{name: "enabled passthrough attempt", setup: firstResponseTimingSetup{enabled: true}, wantHeaders: true},
		{name: "enabled buffered attempt", setup: firstResponseTimingSetup{enabled: true, policy: buffered}, wantHeaders: true},
		{name: "enabled non-stream", setup: firstResponseTimingSetup{enabled: true}, nonStream: true, wantHeaders: true},
		{name: "disabled", setup: firstResponseTimingSetup{}, wantHeaders: false},
		{name: "relay account", setup: firstResponseTimingSetup{enabled: true, relay: true}, wantHeaders: false},
		// 中转上游可能就是另一台开了本开关的 codex2api：它自报的计时头绝不能被
		// 当成本网关的测量转发下去。
		{name: "relay account forwarding an upstream report", setup: firstResponseTimingSetup{enabled: true, relay: true}, forgeTiming: true, wantHeaders: false},
		{name: "relay account with the switch off", setup: firstResponseTimingSetup{relay: true}, forgeTiming: true, wantHeaders: false},
		// 官方路径同理：下发的必须是本网关量出来的值，而不是上游回声。
		{name: "official upstream echoing a report", setup: firstResponseTimingSetup{enabled: true}, forgeTiming: true, wantHeaders: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &firstResponseUpstreamStub{forgeTiming: tc.forgeTiming}
			server := httptest.NewServer(http.HandlerFunc(stub.serve))
			t.Cleanup(server.Close)

			handler, model := newFirstResponseTimingHandler(t, server.URL, tc.setup)
			recorder := httptest.NewRecorder()
			handler.Responses(newFirstResponseTimingContext(model, !tc.nonStream, recorder))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), firstResponseTimingText) {
				t.Fatalf("content missing from the response: %q", recorder.Body.String())
			}
			headers := recorder.Result().Header
			if !tc.wantHeaders {
				assertNoFirstResponseTimingHeaders(t, headers)
				return
			}
			requestMS, _ := parseFirstResponseTimingHeaders(t, headers)
			if strconv.FormatInt(requestMS, 10) == firstResponseTimingForgedMS {
				t.Fatalf("published the upstream's own report instead of this gateway's measurement")
			}
		})
	}
}

func TestUpstreamFirstResponseDoesNotCommitBeforeFirstContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name   string
		policy database.ContinuousRetryPolicy
	}{
		{name: "passthrough attempt"},
		{name: "buffered attempt", policy: database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &firstResponseUpstreamStub{preflight: make(chan struct{}), release: make(chan struct{})}
			server := httptest.NewServer(http.HandlerFunc(stub.serve))
			// Cleanup 是后进先出：放行必须排在 server.Close 之后注册，才能在关闭
			// 桩服务之前先解开卡住的上游写入。
			t.Cleanup(server.Close)
			t.Cleanup(stub.releaseGate)

			var firstDataSent atomic.Bool
			handler, model := newFirstResponseTimingHandler(t, server.URL, firstResponseTimingSetup{enabled: true, policy: tc.policy})
			recorder := newFirstResponseCommitRecorder(&firstDataSent)
			ctx := newFirstResponseTimingContext(model, true, recorder)

			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.Responses(ctx)
			}()

			select {
			case <-stub.preflight:
			case <-time.After(20 * time.Second):
				t.Fatal("stub upstream never delivered the pre-content metadata frame")
			}
			// 元数据已经到达网关；这段静置窗口足以暴露「提前提交 200」的回归。
			time.Sleep(150 * time.Millisecond)
			if committed, _ := recorder.commitState(); committed {
				t.Fatal("pre-content metadata committed the downstream response")
			}

			firstDataSent.Store(true)
			stub.releaseGate()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("Responses never returned")
			}

			committed, afterFirstData := recorder.commitState()
			if !committed {
				t.Fatal("the response was never committed")
			}
			if !afterFirstData {
				t.Fatal("the response was committed before the first content event")
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), firstResponseTimingText) {
				t.Fatalf("content event missing from the stream: %q", recorder.Body.String())
			}
			parseFirstResponseTimingHeaders(t, recorder.Result().Header)
		})
	}
}

func TestUpstreamFirstResponsePublishesOnlyTheWinningAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const firstAttemptDelay = 400 * time.Millisecond

	stub := &firstResponseUpstreamStub{failFirst: true, firstDelay: firstAttemptDelay}
	server := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(server.Close)

	handler, model := newFirstResponseTimingHandler(t, server.URL, firstResponseTimingSetup{
		enabled: true,
		policy: database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryResponseFailed},
		},
	})
	recorder := httptest.NewRecorder()
	handler.Responses(newFirstResponseTimingContext(model, true, recorder))

	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (failed + recovered); body=%q", got, recorder.Body.String())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, firstResponseTimingText) || strings.Contains(body, "temporary upstream failure") {
		t.Fatalf("the failed attempt leaked or the recovered stream is missing: %q", body)
	}

	requestMS, attemptMS := parseFirstResponseTimingHeaders(t, recorder.Result().Header)
	if requestMS < firstAttemptDelay.Milliseconds() {
		t.Fatalf("request timing = %d ms, must include the %d ms failed attempt", requestMS, firstAttemptDelay.Milliseconds())
	}
	if attemptMS >= firstAttemptDelay.Milliseconds() {
		t.Fatalf("attempt timing = %d ms, want only the winning attempt's own elapsed time", attemptMS)
	}
}

// 续想折叠会把 resp 换成最终轮的响应。计时记在 attempt 自己的结构里，所以
// 「缓冲提交 + 续想 + 真的开了续想轮」这一组合也必须照常上报。
func TestUpstreamFirstResponseSurvivesContinueThinkingFold(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(frame string) {
			_, _ = io.WriteString(w, "data: "+frame+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if calls.Add(1) == 1 {
			// reasoning_tokens=516 命中 518n-2 截断指纹：折叠用同一账号开第二轮。
			write(evCreated())
			write(evReasoningAdded(1, 0))
			write(evReasoningDone(2, 0, "enc-round-1"))
			write(evMessageAdded(3, 1))
			write(evMessageDelta(4, 1, "truncated junk"))
			write(evMessageDone(5, 1, "truncated junk"))
			write(evCompleted(6, 100, 600, 516))
			return
		}
		write(evCreated())
		write(evReasoningAdded(1, 0))
		write(evReasoningDone(2, 0, "enc-round-2"))
		write(evMessageAdded(3, 1))
		write(evMessageDelta(4, 1, firstResponseTimingText))
		write(evMessageDone(5, 1, firstResponseTimingText))
		write(evCompleted(6, 120, 900, 400))
	}))
	t.Cleanup(server.Close)

	handler, model := newFirstResponseTimingHandler(t, server.URL, firstResponseTimingSetup{
		enabled:          true,
		policy:           database.ContinuousRetryPolicy{Enabled: true, CatchAll: true},
		continueThinking: true,
	})
	recorder := httptest.NewRecorder()
	handler.Responses(newFirstResponseTimingContext(model, true, recorder))

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream rounds = %d, want 2 (truncated + continuation); body=%q", got, recorder.Body.String())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), firstResponseTimingText) {
		t.Fatalf("the folded answer is missing: %q", recorder.Body.String())
	}
	parseFirstResponseTimingHeaders(t, recorder.Result().Header)
}

func TestUpstreamFirstResponseOmittedWhenKeepaliveAlreadyCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	stub := &firstResponseUpstreamStub{preflight: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(server.Close)
	t.Cleanup(stub.releaseGate)

	handler, model := newFirstResponseTimingHandler(t, server.URL, firstResponseTimingSetup{
		enabled: true,
		policy:  database.ContinuousRetryPolicy{Enabled: true, CatchAll: true},
	})
	recorder := newFirstResponseCommitRecorder(nil)
	ctx := newFirstResponseTimingContext(model, true, recorder)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.Responses(ctx)
	}()

	select {
	case <-stub.preflight:
	case <-time.After(20 * time.Second):
		t.Fatal("stub upstream never delivered the pre-content metadata frame")
	}
	// 首个内容事件仍被卡住，唯一可能提交响应的写入方是重试心跳。
	deadline := time.Now().Add(10 * time.Second)
	for {
		if committed, _ := recorder.commitState(); committed {
			break
		}
		if time.Now().After(deadline) {
			stub.releaseGate()
			<-done
			t.Fatal("the retry keepalive never committed the response headers")
		}
		time.Sleep(5 * time.Millisecond)
	}

	stub.releaseGate()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Responses never returned")
	}

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), continuousRetryKeepaliveComment) {
		t.Fatalf("no keepalive frame was written: %q", recorder.Body.String())
	}
	assertNoFirstResponseTimingHeaders(t, recorder.Result().Header)
}
