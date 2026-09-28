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

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

// wiringPlugin is a no-op transport that proves the core hooks: it answers
// with plugin-namespaced SSE events that only its transformer turns into
// Responses events, so usage can only be extracted after the transform ran.
type wiringPlugin struct {
	executes atomic.Int32
	vetoed   atomic.Bool
	lastEnv  atomic.Pointer[plugins.ReqEnv]
}

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
