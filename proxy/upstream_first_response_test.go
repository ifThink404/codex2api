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

	failedHeaders := make(http.Header)
	failed := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start}
	failed.observe(failedHeaders, gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(100*time.Millisecond))
	if c.Writer.Written() {
		t.Fatal("observing metadata must not commit HTTP 200")
	}
	if len(recorder.Header()) != 0 {
		t.Fatalf("observing metadata staged downstream headers: %v", recorder.Header())
	}

	winnerHeaders := make(http.Header)
	winner := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start.Add(2 * time.Second)}
	for _, event := range []string{"response.created", "response.in_progress", "response.failed", "response.completed", "error", "ping", "keepalive", "heartbeat"} {
		winner.observe(winnerHeaders, gjson.Parse(`{"type":"`+event+`"}`), start.Add(2100*time.Millisecond))
	}
	if len(winnerHeaders) != 0 {
		t.Fatalf("lifecycle/terminal/heartbeat events reported timing: %v", winnerHeaders)
	}
	winner.observe(winnerHeaders, gjson.Parse(`{"type":"response.metadata"}`), start.Add(3*time.Second))
	winner.observe(winnerHeaders, gjson.Parse(`{"type":"response.output_text.delta","delta":"hello"}`), start.Add(10*time.Second))

	relayUpstreamFirstResponseHeaders(c, winnerHeaders)
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
	relayUpstreamFirstResponseHeaders(c, failedHeaders)
	if got := result.Header.Get(upstreamFirstResponseHeader); got != "3000" {
		t.Fatalf("committed request timing changed to %q", got)
	}
}

func TestUpstreamFirstResponseDisabledOrCommittedFallsBack(t *testing.T) {
	start := time.Now()
	headers := make(http.Header)
	disabled := upstreamFirstResponseTiming{requestStart: start, attemptStart: start}
	disabled.observe(headers, gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(time.Second))
	if len(headers) != 0 {
		t.Fatalf("disabled timing reported headers: %v", headers)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Writer.WriteHeaderNow() // 已有心跳提前提交了响应头。
	enabled := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start}
	enabled.observe(headers, gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(time.Second))
	relayUpstreamFirstResponseHeaders(c, headers)
	if got := recorder.Result().Header.Get(upstreamTimingHeader); got != "" {
		t.Fatalf("committed response still reported timing: %q", got)
	}
}

func TestUpstreamFirstResponseTimingRecordsLooseFirstTokenOnce(t *testing.T) {
	start := time.Now()
	headers := make(http.Header)
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
		timing.observe(headers, gjson.Parse(event), start.Add(time.Second))
		if len(headers) != 0 {
			t.Fatalf("event %s reported timing: %v", event, headers)
		}
	}

	timing.observe(headers, gjson.Parse(`{"type":"response.output_text.delta","delta":"hi"}`), start.Add(2*time.Second))
	if got := headers.Get(upstreamTimingHeader); got != "v1-loose" {
		t.Fatalf("timing version = %q, want v1-loose", got)
	}
	if got := headers.Get(upstreamFirstResponseHeader); got != "2000" {
		t.Fatalf("request timing = %q, want 2000", got)
	}
	if got := headers.Get(upstreamAttemptFirstResponseHeader); got != "2000" {
		t.Fatalf("attempt timing = %q, want 2000", got)
	}

	timing.observe(headers, gjson.Parse(`{"type":"response.output_text.delta","delta":"later"}`), start.Add(9*time.Second))
	if got := headers.Get(upstreamFirstResponseHeader); got != "2000" {
		t.Fatalf("a later content event rewrote the recorded timing: %q", got)
	}
}

func TestUpstreamFirstResponseAttemptNeverExceedsRequest(t *testing.T) {
	now := time.Now()
	// 换号后 attempt 时钟可能比请求级时钟更早；attempt 值必须被夹到请求值以内，
	// 否则「快的替身」会看起来比整条请求还快。
	clamped := make(http.Header)
	timing := upstreamFirstResponseTiming{enabled: true, requestStart: now.Add(500 * time.Millisecond), attemptStart: now}
	timing.observe(clamped, gjson.Parse(`{"type":"codex.rate_limits"}`), now.Add(time.Second))
	if got := clamped.Get(upstreamFirstResponseHeader); got != "500" {
		t.Fatalf("request timing = %q, want 500", got)
	}
	if got := clamped.Get(upstreamAttemptFirstResponseHeader); got != "500" {
		t.Fatalf("attempt timing = %q, want 500 (clamped to the request value)", got)
	}

	negative := make(http.Header)
	future := upstreamFirstResponseTiming{enabled: true, requestStart: now.Add(2 * time.Second), attemptStart: now.Add(2 * time.Second)}
	future.observe(negative, gjson.Parse(`{"type":"codex.rate_limits"}`), now)
	if got := negative.Get(upstreamFirstResponseHeader); got != "0" {
		t.Fatalf("request timing = %q, want 0 for a non-positive elapsed duration", got)
	}
	if got := negative.Get(upstreamAttemptFirstResponseHeader); got != "0" {
		t.Fatalf("attempt timing = %q, want 0 for a non-positive elapsed duration", got)
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

func TestRelayUpstreamFirstResponseHeadersDropsStaleAttemptValues(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Header(upstreamTimingHeader, "v1-loose")
	c.Header(upstreamFirstResponseHeader, "999")
	c.Header(upstreamAttemptFirstResponseHeader, "999")

	// 上一 attempt 的计时不能粘到换号后的新响应上。
	relayUpstreamFirstResponseHeaders(c, make(http.Header))
	for _, name := range []string{upstreamTimingHeader, upstreamFirstResponseHeader, upstreamAttemptFirstResponseHeader} {
		if got := c.Writer.Header().Get(name); got != "" {
			t.Fatalf("stale %s survived a timing-free attempt: %q", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 端到端：真实 handler + 桩上游
// ---------------------------------------------------------------------------

const firstResponseTimingCodexModel = "gpt-5.5"
const firstResponseTimingRelayModel = "gpt-4.1-direct"

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
	write(`{"type":"response.output_text.delta","delta":"first-response-hello"}`)
	write(`{"type":"response.completed","response":{"id":"resp_timing","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
}

// newFirstResponseTimingHandler 装配「运行期开关 + 号池 + 桩上游出口」。relay 为
// true 时使用中转账号（不参与本计时契约），否则走官方 Codex OAuth 出口。
func newFirstResponseTimingHandler(t *testing.T, upstreamURL string, enabled bool, policy database.ContinuousRetryPolicy, relay bool) (*Handler, string) {
	t.Helper()
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	next := DefaultRuntimeSettings()
	next.CodexForceWebsocket = false
	next.CodexWSSilentRetry = false
	next.CodexWSSilentRetries = 0
	next.CodexPreflightSSEPassthrough = enabled
	next.ContinuousRetryPolicy = policy
	ApplyRuntimeSettings(next)

	if relay {
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

func newFirstResponseTimingContext(model string, writer http.ResponseWriter) *gin.Context {
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"`+model+`","input":"hello","stream":true}`,
	))
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
		enabled     bool
		policy      database.ContinuousRetryPolicy
		relay       bool
		wantHeaders bool
	}{
		{name: "enabled passthrough attempt", enabled: true, wantHeaders: true},
		{name: "enabled buffered attempt", enabled: true, policy: buffered, wantHeaders: true},
		{name: "disabled", enabled: false, wantHeaders: false},
		{name: "relay account", enabled: true, relay: true, wantHeaders: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &firstResponseUpstreamStub{}
			server := httptest.NewServer(http.HandlerFunc(stub.serve))
			t.Cleanup(server.Close)

			handler, model := newFirstResponseTimingHandler(t, server.URL, tc.enabled, tc.policy, tc.relay)
			recorder := httptest.NewRecorder()
			handler.Responses(newFirstResponseTimingContext(model, recorder))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "first-response-hello") {
				t.Fatalf("content event missing from the stream: %q", recorder.Body.String())
			}
			headers := recorder.Result().Header
			if !tc.wantHeaders {
				assertNoFirstResponseTimingHeaders(t, headers)
				return
			}
			requestMS, attemptMS := parseFirstResponseTimingHeaders(t, headers)
			if requestMS > time.Minute.Milliseconds() {
				t.Fatalf("request timing = %d ms, implausible for a local stub upstream", requestMS)
			}
			_ = attemptMS
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
			handler, model := newFirstResponseTimingHandler(t, server.URL, true, tc.policy, false)
			recorder := newFirstResponseCommitRecorder(&firstDataSent)
			ctx := newFirstResponseTimingContext(model, recorder)

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
			if !strings.Contains(recorder.Body.String(), "first-response-hello") {
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

	handler, model := newFirstResponseTimingHandler(t, server.URL, true, database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}, false)
	recorder := httptest.NewRecorder()
	handler.Responses(newFirstResponseTimingContext(model, recorder))

	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (failed + recovered); body=%q", got, recorder.Body.String())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "first-response-hello") || strings.Contains(body, "temporary upstream failure") {
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

func TestUpstreamFirstResponseOmittedWhenKeepaliveAlreadyCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })

	stub := &firstResponseUpstreamStub{preflight: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(server.Close)
	t.Cleanup(stub.releaseGate)

	handler, model := newFirstResponseTimingHandler(t, server.URL, true, database.ContinuousRetryPolicy{
		Enabled: true, CatchAll: true,
	}, false)
	recorder := newFirstResponseCommitRecorder(nil)
	ctx := newFirstResponseTimingContext(model, recorder)

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
