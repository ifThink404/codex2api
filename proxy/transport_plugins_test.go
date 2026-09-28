package proxy

import (
	"bytes"
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
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// wiringPlugin is a no-op transport that proves the core hooks: it answers
// with plugin-namespaced SSE events that only its transformer turns into
// Responses events, so usage can only be extracted after the transform ran.
type wiringPlugin struct {
	executes atomic.Int32
	vetoed   atomic.Bool
	lastEnv  atomic.Pointer[plugins.ReqEnv]
	// errorKind, when set, is recorded as the usage row's error kind.
	errorKind string
	// spare is the plugin's native-health policy; failStatus/failBody make
	// Execute answer with an HTTP error, failEvent with a failed stream.
	spare      atomic.Bool
	failStatus int
	failBody   string
	failEvent  string
}

func (p *wiringPlugin) SparesNativeHealth() bool { return p.spare.Load() }

func (p *wiringPlugin) ID() string { return "wiringplug" }
func (p *wiringPlugin) Describe() plugins.Meta {
	return plugins.Meta{Name: "wiring", Kinds: []plugins.RequestKind{plugins.KindResponses}, UpstreamEndpoint: "/wiring/responses"}
}
func (p *wiringPlugin) Admissible(context.Context, *auth.Account, string) (bool, string) {
	return !p.vetoed.Load(), "vetoed"
}
func (p *wiringPlugin) Select(context.Context, plugins.Attempt) bool { return true }
func (p *wiringPlugin) Execute(_ context.Context, env *plugins.ReqEnv) (*http.Response, error) {
	p.executes.Add(1)
	p.lastEnv.Store(env)
	env.Request.SetUsageMeta(p.ID(), `{"profile":"test"}`)
	if p.errorKind != "" {
		env.Request.SetUsageErrorKind(p.ID(), p.errorKind)
	}
	if p.failStatus != 0 {
		return &http.Response{StatusCode: p.failStatus, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(p.failBody))}, nil
	}
	if p.failEvent != "" {
		stream := "data: {\"type\":\"plugin.created\",\"response\":{\"id\":\"resp_p\",\"status\":\"in_progress\"}}\n\ndata: " + p.failEvent + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}
	var body bytes.Buffer
	for _, event := range []string{
		`{"type":"plugin.created","response":{"id":"resp_p","status":"in_progress"}}`,
		`{"type":"plugin.output_text.delta","delta":"OK"}`,
		`{"type":"plugin.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`,
		`{"type":"plugin.completed","response":{"id":"resp_p","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":123,"output_tokens":5,"total_tokens":128}}}`,
	} {
		body.WriteString("data: " + event + "\n\n")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Plugin-Internal": {"1"}}, Body: io.NopCloser(&body)}, nil
}
func (p *wiringPlugin) FilterHeaders(_ *plugins.ReqEnv, h http.Header) { h.Del("X-Plugin-Internal") }
func (p *wiringPlugin) TransformJSON(_ *plugins.ReqEnv, _ int, body []byte) ([]byte, error) {
	return body, nil
}
func (p *wiringPlugin) TransformSSEFrame(_ *plugins.ReqEnv, event string, data []byte) ([]plugins.SSEFrame, error) {
	return []plugins.SSEFrame{{Event: event, Data: bytes.ReplaceAll(data, []byte(`"type":"plugin.`), []byte(`"type":"response.`))}}, nil
}

func newTransportPluginTestHandler(t *testing.T, enabled bool) (*Handler, *database.DB, *wiringPlugin, *atomic.Int32) {
	t.Helper()
	handler, nativeCalls := newChatStreamServeTestHandler(t, writeSuccessfulAttempt)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "wiring.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetUsageLogConfig(database.UsageLogModeFull, 100, 300)
	handler = NewHandler(handler.store, db, &config.Config{AllowAnonymousV1: true}, nil)

	registry := plugins.NewRegistry()
	plugin := &wiringPlugin{}
	registry.Register(plugin)
	if err := registry.Attach(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	previous := plugins.SwapDefault(registry)
	t.Cleanup(func() {
		plugins.SwapDefault(previous)
		auth.SetTransportPluginReloader(nil)
	})
	if err := registry.Save(context.Background(), database.TransportPluginState{ID: plugin.ID(), Enabled: enabled}); err != nil {
		t.Fatal(err)
	}
	return handler, db, plugin, nativeCalls
}

// invokeTracedResponses attaches the request trace like the /v1 middleware,
// so the plugin request and usage row carry a request_id.
func invokeTracedResponses(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	attachUpstreamTrace(ctx, handler.store)
	ctx.Request = ctx.Request.WithContext(withUserAgentAudit(ctx.Request.Context()))
	handler.Responses(ctx)
	return recorder
}

func onlyUsageLog(t *testing.T, db *database.DB) *database.UsageLog {
	t.Helper()
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("usage logs = %d, want 1", len(logs))
	}
	return logs[0]
}

func TestResponsesTransportPluginServesAttempt(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "non-stream"}[stream], func(t *testing.T) {
			handler, db, plugin, nativeCalls := newTransportPluginTestHandler(t, true)
			recorder := invokeTracedResponses(t, handler, `{"model":"gpt-5.5","stream":`+boolString(stream)+`,"input":"hi"}`)
			got := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(got, `"OK"`) || strings.Contains(got, "plugin.") {
				t.Fatalf("status=%d body=%q", recorder.Code, got)
			}
			if recorder.Header().Get("X-Plugin-Internal") != "" {
				t.Fatal("plugin header filter not applied")
			}
			if plugin.executes.Load() != 1 || nativeCalls.Load() != 0 {
				t.Fatalf("plugin executes=%d native calls=%d", plugin.executes.Load(), nativeCalls.Load())
			}
			env := plugin.lastEnv.Load()
			if env == nil || env.Account == nil || env.Model != "gpt-5.5" || !strings.Contains(string(env.Body), `"input"`) || env.Attempt != 1 || env.Request == nil || env.Request.ID == "" {
				t.Fatalf("env account=%v model=%q attempt=%d request=%+v", env.Account, env.Model, env.Attempt, env.Request)
			}
			row := onlyUsageLog(t, db)
			if row.Transport != plugin.ID() || row.PluginMeta != `{"profile":"test"}` || row.UpstreamEndpoint != "/wiring/responses" || row.InputTokens != 123 || row.RequestID != env.Request.ID {
				t.Fatalf("usage row transport=%q meta=%q endpoint=%q input=%d rid=%q", row.Transport, row.PluginMeta, row.UpstreamEndpoint, row.InputTokens, row.RequestID)
			}
		})
	}
}

func TestResponsesTransportPluginRecordsUsageErrorKind(t *testing.T) {
	handler, db, plugin, _ := newTransportPluginTestHandler(t, true)
	plugin.errorKind = "plugin_marker"
	recorder := invokeTracedResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":"hi"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	row := onlyUsageLog(t, db)
	if row.Transport != plugin.ID() || row.StatusCode != http.StatusOK || row.UpstreamErrorKind != "plugin_marker" {
		t.Fatalf("usage row transport=%q status=%d kind=%q", row.Transport, row.StatusCode, row.UpstreamErrorKind)
	}
}

func TestResponsesTransportPluginDisabledStaysNative(t *testing.T) {
	handler, db, plugin, nativeCalls := newTransportPluginTestHandler(t, false)
	recorder := invokeResponsesWithBody(t, handler, `{"model":"gpt-5.5","stream":true,"input":"hi"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if plugin.executes.Load() != 0 || nativeCalls.Load() != 1 {
		t.Fatalf("plugin executes=%d native calls=%d", plugin.executes.Load(), nativeCalls.Load())
	}
	if row := onlyUsageLog(t, db); row.Transport != database.TransportNative || row.UpstreamEndpoint != "/v1/responses" {
		t.Fatalf("usage row transport=%q endpoint=%q", row.Transport, row.UpstreamEndpoint)
	}
}

func TestResponsesTransportPluginAdmissibleVeto(t *testing.T) {
	handler, _, plugin, nativeCalls := newTransportPluginTestHandler(t, true)
	plugin.vetoed.Store(true)
	recorder := invokeResponsesWithBody(t, handler, `{"model":"gpt-5.5","stream":false,"input":"hi"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 with the only account vetoed; body=%q", recorder.Code, recorder.Body.String())
	}
	if plugin.executes.Load() != 0 || nativeCalls.Load() != 0 {
		t.Fatalf("vetoed account still dispatched: plugin=%d native=%d", plugin.executes.Load(), nativeCalls.Load())
	}
}

// compactWiringPlugin serves /v1/responses/compact with a JSON body that only
// its transformer completes.
type compactWiringPlugin struct {
	wiringPlugin
}

func (p *compactWiringPlugin) Describe() plugins.Meta {
	return plugins.Meta{Name: "compact wiring", Kinds: []plugins.RequestKind{plugins.KindResponsesCompact}}
}
func (p *compactWiringPlugin) Execute(_ context.Context, env *plugins.ReqEnv) (*http.Response, error) {
	p.executes.Add(1)
	p.lastEnv.Store(env)
	body := `{"id":"resp_compact_plugin","object":"PLACEHOLDER","output":[{"type":"compaction","encrypted_content":"enc"}],"usage":{"input_tokens":50,"output_tokens":7,"total_tokens":57}}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func (p *compactWiringPlugin) TransformJSON(_ *plugins.ReqEnv, _ int, body []byte) ([]byte, error) {
	return bytes.Replace(body, []byte("PLACEHOLDER"), []byte("response.compaction"), 1), nil
}

func TestResponsesCompactTransportPluginServesAttempt(t *testing.T) {
	handler, nativeCalls := newChatStreamServeTestHandler(t, writeSuccessfulAttempt)
	registry := plugins.NewRegistry()
	plugin := &compactWiringPlugin{}
	registry.Register(plugin)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "compact.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := registry.Attach(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	previous := plugins.SwapDefault(registry)
	t.Cleanup(func() {
		plugins.SwapDefault(previous)
		auth.SetTransportPluginReloader(nil)
	})
	if err := registry.Save(context.Background(), database.TransportPluginState{ID: plugin.ID(), Enabled: true}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5.5","input":"hello"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"object":"response.compaction"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	env := plugin.lastEnv.Load()
	if plugin.executes.Load() != 1 || nativeCalls.Load() != 0 || env == nil || !env.Compact {
		t.Fatalf("plugin executes=%d native=%d env=%+v", plugin.executes.Load(), nativeCalls.Load(), env)
	}
}

// servicesPlugin performs its upstream call only through env.Services, as a
// plugin outside this package must.
type servicesPlugin struct {
	wiringPlugin
	sawServices atomic.Bool
}

func (p *servicesPlugin) Describe() plugins.Meta {
	return plugins.Meta{Name: "services", Kinds: []plugins.RequestKind{plugins.KindResponses}}
}

func (p *servicesPlugin) Execute(ctx context.Context, env *plugins.ReqEnv) (*http.Response, error) {
	p.executes.Add(1)
	svc := env.Services
	p.sawServices.Store(svc != nil)
	if svc == nil {
		return nil, context.Canceled
	}
	if err := svc.ConsumeModelQuota(ctx, env.Model); err != nil {
		return nil, err
	}
	client, rewrite := svc.HTTPClient(env.Account, env.ProxyURL)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, rewrite("https://plugin.example/responses"), bytes.NewReader(env.Body))
	req.Header.Set("User-Agent", "PluginAgent/1")
	svc.PrepareRequest(req, env.Account)
	svc.RecordUserAgent(ctx, req.Header.Get("User-Agent"))
	return svc.Do(client, req, env.Account, env.ProxyURL)
}

func TestTransportPluginServicesAuditLikeNative(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	handler, db, _, nativeCalls := newTransportPluginTestHandler(t, true)
	registry := plugins.NewRegistry()
	plugin := &servicesPlugin{}
	registry.Register(plugin)
	require.NoError(t, registry.Attach(context.Background(), db))
	previous := plugins.SwapDefault(registry)
	t.Cleanup(func() { plugins.SwapDefault(previous) })
	require.NoError(t, registry.Save(context.Background(), database.TransportPluginState{ID: plugin.ID(), Enabled: true}))

	previousResin := resinCfg.Load()
	SetResinConfig(nil)
	t.Cleanup(func() { SetResinConfig(previousResin) })
	account := handler.store.FindByID(1)
	var sentUA string
	installClaudeBoundaryTransport(t, account, func(r *http.Request) (*http.Response, error) {
		sentUA = r.Header.Get("User-Agent")
		sse := "data: " + `{"type":"response.completed","response":{"id":"resp_s","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}` + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"req-upstream-7"}}, Body: io.NopCloser(strings.NewReader(sse)), Request: r}, nil
	})
	recorder := invokeTracedResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":"hi"}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.True(t, plugin.sawServices.Load(), "Route.Execute hands the plugin the core services")
	require.Zero(t, nativeCalls.Load())
	require.Equal(t, "PluginAgent/1", sentUA, "the pooled client for the account carried the request")
	row := onlyUsageLog(t, db)
	require.Equal(t, plugin.ID(), row.Transport)
	require.Equal(t, "PluginAgent/1", row.UpstreamUserAgent, "UA audit")
	require.Equal(t, "req-upstream-7", row.UpstreamRequestID, "trace begin/finish recorded the upstream request id")
}

func dialPluginWS(t *testing.T, handler *Handler) *websocket.Conn {
	t.Helper()
	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A plugin serving a downstream WebSocket turn runs over HTTP through the
// plugin; the native WS executor is never used, and Admissible still vetoes.
func TestResponsesWebSocketTurnUsesTransportPlugin(t *testing.T) {
	previousExec := WebsocketExecuteFunc
	var wsCalls atomic.Int32
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		wsCalls.Add(1)
		return nil, context.Canceled
	}
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	handler, db, plugin, nativeCalls := newTransportPluginTestHandler(t, true)

	conn := dialPluginWS(t, handler)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.5","input":"hello"}`)))
	for {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
		_, frame, err := conn.ReadMessage()
		require.NoError(t, err)
		kind := gjson.GetBytes(frame, "type").String()
		require.NotContains(t, kind, "plugin.", "frames are transformed before the client sees them")
		if kind == "response.completed" {
			break
		}
	}
	require.EqualValues(t, 1, plugin.executes.Load())
	require.Zero(t, wsCalls.Load())
	require.Zero(t, nativeCalls.Load())
	require.Eventually(t, func() bool {
		db.FlushUsageLogs()
		logs, err := db.ListRecentUsageLogs(context.Background(), 5)
		return err == nil && len(logs) == 1 && logs[0].Transport == plugin.ID()
	}, 3*time.Second, 20*time.Millisecond, "WS turn usage row carries the plugin transport")

	plugin.vetoed.Store(true)
	vetoed := dialPluginWS(t, handler)
	require.NoError(t, vetoed.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.5","input":"again"}`)))
	require.NoError(t, vetoed.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, frame, err := vetoed.ReadMessage()
	if err == nil {
		require.NotEqual(t, "response.completed", gjson.GetBytes(frame, "type").String())
	}
	require.EqualValues(t, 1, plugin.executes.Load(), "the only account is vetoed on the WS path too")
	require.Zero(t, wsCalls.Load())
}
