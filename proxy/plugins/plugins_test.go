package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// testPlugin is a no-op transport used only to prove core wiring.
type testPlugin struct {
	id        string
	kinds     []RequestKind
	veto      atomic.Bool
	selectAll bool
	binds     atomic.Int32
	executes  atomic.Int32
	status    int
	ctype     string
	body      string
	execErr   error
	transform bool
}

func (p *testPlugin) ID() string { return p.id }
func (p *testPlugin) Describe() Meta {
	return Meta{Name: p.id, Kinds: p.kinds, UpstreamEndpoint: "/test/responses"}
}
func (p *testPlugin) Admissible(context.Context, *auth.Account, string) (bool, string) {
	if p.veto.Load() {
		return false, "vetoed"
	}
	return true, ""
}
func (p *testPlugin) Select(_ context.Context, a Attempt) bool { return p.selectAll }
func (p *testPlugin) BindRequest(req *Request)                 { p.binds.Add(1); req.SetState(p.id, "bound") }
func (p *testPlugin) Execute(_ context.Context, env *ReqEnv) (*http.Response, error) {
	p.executes.Add(1)
	if p.execErr != nil {
		return nil, p.execErr
	}
	header := http.Header{"Content-Type": {p.ctype}, "X-Upstream-Secret": {"s"}, "Authorization": {"Bearer sk-abcdefghijklmnopqrstuvwxyz"}}
	return &http.Response{StatusCode: p.status, Header: header, Body: io.NopCloser(strings.NewReader(p.body))}, nil
}

type transformingPlugin struct{ *testPlugin }

func (transformingPlugin) FilterHeaders(_ *ReqEnv, h http.Header) { h.Del("X-Upstream-Secret") }
func (transformingPlugin) TransformJSON(_ *ReqEnv, _ int, body []byte) ([]byte, error) {
	return bytes.ReplaceAll(body, []byte("bps"), []byte("responses")), nil
}
func (transformingPlugin) TransformSSEFrame(_ *ReqEnv, event string, data []byte) ([]SSEFrame, error) {
	if event == "bps.internal" {
		return nil, nil
	}
	return []SSEFrame{{Event: strings.Replace(event, "bps.", "response.", 1), Data: bytes.ReplaceAll(data, []byte("bps"), []byte("responses"))}}, nil
}

func newTestStore(t *testing.T) (*database.DB, *auth.Store) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"})
	t.Cleanup(func() {
		store.Stop()
		auth.SetTransportPluginReloader(nil)
		_ = db.Close()
	})
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db, store
}

func insertAccount(t *testing.T, db *database.DB, store *auth.Store, creds map[string]interface{}) *auth.Account {
	t.Helper()
	base := map[string]interface{}{"upstream_type": auth.UpstreamOpenAIResponses, "base_url": "https://p.example", "api_key": "sk-p", "models": []string{"gpt-5.6"}}
	for k, v := range creds {
		base[k] = v
	}
	id, err := db.InsertOpenAIResponsesAccount(context.Background(), "p", base, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return store.FindByID(id) != nil })
	return store.FindByID(id)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestEnablementPrecedence(t *testing.T) {
	db, store := newTestStore(t)
	reg := NewRegistry()
	p := &testPlugin{id: "precplug", kinds: []RequestKind{KindResponses}}
	reg.Register(p)
	key := OverrideCredentialKey(p)
	if key != "transport_plugin_precplug_enabled" {
		t.Fatalf("override key = %q", key)
	}
	if err := reg.Attach(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	plain := insertAccount(t, db, store, nil)
	grouped := insertAccount(t, db, store, nil)
	forcedOff := insertAccount(t, db, store, map[string]interface{}{key: false})
	forcedOn := insertAccount(t, db, store, map[string]interface{}{key: true})
	const groupID = int64(99)
	store.ApplyAccountGroups(grouped.ID(), []int64{groupID})
	store.ApplyAccountGroups(forcedOff.ID(), []int64{groupID})

	check := func(label string, want map[*auth.Account]bool) {
		t.Helper()
		for account, enabled := range want {
			if got := reg.EnabledFor(p, account); got != enabled {
				t.Fatalf("%s: account %d enabled = %v, want %v", label, account.ID(), got, enabled)
			}
		}
	}
	check("default off", map[*auth.Account]bool{plain: false, grouped: false, forcedOff: false, forcedOn: true})

	save := func(state database.TransportPluginState) {
		t.Helper()
		state.ID = p.id
		if err := reg.Save(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}
	save(database.TransportPluginState{GroupIDs: []int64{groupID}})
	check("group", map[*auth.Account]bool{plain: false, grouped: true, forcedOff: false, forcedOn: true})
	save(database.TransportPluginState{Enabled: true})
	check("global", map[*auth.Account]bool{plain: true, grouped: true, forcedOff: false, forcedOn: true})
	if reg.EnabledFor(p, nil) {
		t.Fatal("nil account enabled")
	}
	if err := reg.Save(context.Background(), database.TransportPluginState{ID: "unknown"}); err == nil {
		t.Fatal("saving an unregistered plugin succeeded")
	}
}

// A second registry attached to a second store stands in for another
// replica: it only learns about the change through the scheduler outbox.
func TestStateHotReloadViaOutbox(t *testing.T) {
	db, store := newTestStore(t)
	reg := NewRegistry()
	p := &testPlugin{id: "reloadplug", kinds: []RequestKind{KindResponses}}
	reg.Register(p)
	if err := reg.Attach(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	account := insertAccount(t, db, store, nil)
	if reg.EnabledFor(p, account) {
		t.Fatal("enabled before any state")
	}
	if err := db.SaveTransportPluginState(context.Background(), database.TransportPluginState{ID: p.id, Enabled: true, CaptureEnabled: true, CaptureSampleRate: 0.5}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return reg.EnabledFor(p, account) })
	if state := reg.State(p.id); !state.CaptureEnabled || state.CaptureSampleRate != 0.5 {
		t.Fatalf("reloaded state = %+v", state)
	}
	if err := db.SaveTransportPluginState(context.Background(), database.TransportPluginState{ID: p.id}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !reg.EnabledFor(p, account) })
}

func TestResolveAndAccountFilter(t *testing.T) {
	reg := NewRegistry()
	p := &testPlugin{id: "resolveplug", kinds: []RequestKind{KindResponses}, selectAll: true}
	reg.Register(p)
	reg.applyStates([]database.TransportPluginState{{ID: p.id, Enabled: true}})
	account := &auth.Account{DBID: 42}

	req := NewRequest("req-1", KindResponses, nil, nil, 0)
	if route := reg.Resolve(context.Background(), req, account, "m", KindResponsesCompact); route != nil {
		t.Fatal("plugin served an unsupported kind")
	}
	if transport, _ := req.Transport(); transport != database.TransportNative {
		t.Fatalf("transport = %q, want native", transport)
	}
	route := reg.Resolve(context.Background(), req, account, "m", KindResponses)
	if route == nil || route.ID() != p.id {
		t.Fatalf("route = %v", route)
	}
	route = reg.Resolve(context.Background(), req, account, "m", KindResponses)
	if route.attempt.Index != 3 || route.attempt.Prior != p.id {
		t.Fatalf("attempt = %+v", route.attempt)
	}
	if p.binds.Load() != 1 || req.State(p.id) != "bound" {
		t.Fatalf("BindRequest ran %d times", p.binds.Load())
	}
	req.SetUsageMeta(p.id, `{"x":1}`)
	if transport, meta := req.Transport(); transport != p.id || meta != `{"x":1}` {
		t.Fatalf("transport/meta = %q/%q", transport, meta)
	}

	base := func(a *auth.Account) bool { return a.ID() != 7 }
	filter := reg.AccountFilter(context.Background(), req, KindResponses, "m", base)
	if !filter(account) || filter(&auth.Account{DBID: 7}) {
		t.Fatal("filter did not respect base filter")
	}
	p.veto.Store(true)
	if filter(account) {
		t.Fatal("Admissible veto ignored")
	}
	if reg.AccountFilter(context.Background(), req, KindResponsesCompact, "m", nil) != nil {
		t.Fatal("a kind no plugin supports must leave the filter unchanged")
	}
	empty := NewRegistry()
	if got := empty.AccountFilter(context.Background(), req, KindResponses, "m", nil); got != nil {
		t.Fatal("registry without plugins must not wrap the filter")
	}
}

func TestSSETransformReader(t *testing.T) {
	src := "event: bps.delta\ndata: {\"t\":\"bps\"}\n\n" +
		": keepalive\n\n" +
		"event: bps.internal\ndata: secret\n\n" +
		"event: response.completed\r\ndata: {\"ok\":true}\r\n\r\n" +
		"data: line1\ndata: bps2\n\n" +
		"data: tail-without-blank"
	tp := transformingPlugin{&testPlugin{}}
	r := newSSETransformReader(io.NopCloser(strings.NewReader(src)), func(event string, data []byte) ([]SSEFrame, error) {
		return tp.TransformSSEFrame(nil, event, data)
	}, func() ([]SSEFrame, error) { return []SSEFrame{{Event: "done", Data: []byte("{}")}}, nil })
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	want := "event: response.delta\ndata: {\"t\":\"responses\"}\n\n" +
		": keepalive\n\n" +
		"event: response.completed\r\ndata: {\"ok\":true}\r\n\r\n" +
		"data: line1\ndata: responses2\n\n" +
		"data: tail-without-blank" +
		"event: done\ndata: {}\n\n"
	if string(out) != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out, want)
	}
}

type memorySink struct {
	mu       chan struct{}
	captures []database.PluginCapture
}

func (s *memorySink) InsertPluginCaptures(_ context.Context, captures []database.PluginCapture) error {
	s.mu <- struct{}{}
	s.captures = append(s.captures, captures...)
	<-s.mu
	return nil
}

func (s *memorySink) snapshot() []database.PluginCapture {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	return append([]database.PluginCapture(nil), s.captures...)
}

func TestRouteExecuteTransformsAndCaptures(t *testing.T) {
	reg := NewRegistry()
	base := &testPlugin{id: "execplug", kinds: []RequestKind{KindResponses}, selectAll: true, status: 200, ctype: "application/json", body: `{"object":"bps","access_token":"tok-123"}`}
	reg.Register(transformingPlugin{base})
	reg.applyStates([]database.TransportPluginState{{ID: base.id, Enabled: true, CaptureEnabled: true, CaptureSampleRate: 1, Config: []byte(`{"c":1}`)}})
	sink := &memorySink{mu: make(chan struct{}, 1)}
	reg.capture.setSink(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.StartCaptureWriter(ctx)

	req := NewRequest("req-exec", KindResponses, nil, nil, 0)
	route := reg.Resolve(context.Background(), req, &auth.Account{DBID: 9}, "m", KindResponses)
	resp, err := route.Execute(context.Background(), ReqEnv{Body: []byte(`{"input":"hi","api_key":"sk-abcdefghijklmnopqrstuvwxyz"}`), Header: http.Header{"Authorization": {"Bearer x"}, "User-Agent": {"codex"}}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"object":"responses","access_token":"tok-123"}` || resp.Header.Get("X-Upstream-Secret") != "" {
		t.Fatalf("transformed = %s headers=%v", body, resp.Header)
	}
	waitFor(t, func() bool { return len(sink.snapshot()) == 2 })
	captures := sink.snapshot()
	byDir := map[string]database.PluginCapture{}
	for _, c := range captures {
		byDir[c.Direction] = c
		if c.Plugin != base.id || c.RequestID != "req-exec" || c.AccountID != 9 || c.Attempt != 1 {
			t.Fatalf("capture identity = %+v", c)
		}
	}
	reqCap, respCap := byDir[database.PluginCaptureDirectionRequest], byDir[database.PluginCaptureDirectionResponse]
	if strings.Contains(reqCap.Headers, "Bearer x") || !strings.Contains(reqCap.Headers, "[REDACTED]") || strings.Contains(reqCap.Body, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("request capture not masked: %+v", reqCap)
	}
	// Response captures hold the raw upstream body, before the transform.
	if !strings.Contains(respCap.Body, `"object":"bps"`) || strings.Contains(respCap.Body, "tok-123") || respCap.Status != 200 || strings.Contains(respCap.Headers, "sk-abcdefghij") {
		t.Fatalf("response capture = %+v", respCap)
	}

	base.execErr = errors.New("dial failed")
	if _, err := route.Execute(context.Background(), ReqEnv{}); err == nil {
		t.Fatal("execute error swallowed")
	}
	waitFor(t, func() bool { return len(sink.snapshot()) == 4 })
	if last := sink.snapshot()[3]; last.Direction != database.PluginCaptureDirectionError || last.ErrorKind != "execute_error" {
		t.Fatalf("error capture = %+v", last)
	}
}

func TestCaptureOffWithoutSwitchOrSample(t *testing.T) {
	w := newCaptureWriter()
	w.setSink(&memorySink{mu: make(chan struct{}, 1)})
	env := &ReqEnv{Request: NewRequest("r", KindResponses, nil, nil, 0)}
	if w.begin(database.TransportPluginState{ID: "x", CaptureSampleRate: 1}, env) != nil {
		t.Fatal("capture without switch")
	}
	if w.begin(database.TransportPluginState{ID: "x", CaptureEnabled: true}, env) != nil {
		t.Fatal("capture at rate 0")
	}
	var nilRec *captureRecorder
	nilRec.request(env)
	nilRec.failure(errors.New("x"))
	nilRec.response(&http.Response{})
}

func TestCaptureSamplingIsStablePerRequest(t *testing.T) {
	hits := 0
	for i := 0; i < 10000; i++ {
		id := "req-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + time.Duration(i).String()
		first := captureSampled(id, 0.1)
		if first != captureSampled(id, 0.1) {
			t.Fatal("sampling is not deterministic per request id")
		}
		if first {
			hits++
		}
	}
	if hits < 700 || hits > 1300 {
		t.Fatalf("10%% sampling hit %d/10000", hits)
	}
	if captureSampled("any", 0) || !captureSampled("any", 1) {
		t.Fatal("rate bounds")
	}
}

func TestCaptureMaskingAndTruncation(t *testing.T) {
	big := strings.Repeat("é", database.PluginCaptureBodyLimit) // 2 bytes per rune
	body, truncated := maskCaptureBody([]byte(`{"refresh_token":"rt-secret"}` + big))
	if !truncated || len(body) > database.PluginCaptureBodyLimit || strings.Contains(body, "rt-secret") {
		t.Fatalf("truncated=%v len=%d", truncated, len(body))
	}
	if !strings.HasSuffix(body, "é") {
		t.Fatal("truncation split a UTF-8 sequence")
	}
	small, truncated := maskCaptureBody([]byte("plain"))
	if truncated || small != "plain" {
		t.Fatalf("small = %q %v", small, truncated)
	}
	headers := maskCaptureHeaders(http.Header{"Cookie": {"a=b"}, "X-Trace": {"Bearer abc.def"}})
	if strings.Contains(headers, "a=b") || strings.Contains(headers, "abc.def") {
		t.Fatalf("headers not masked: %s", headers)
	}

	tee := &captureTee{src: io.NopCloser(strings.NewReader(strings.Repeat("z", database.PluginCaptureBodyLimit+10))), rec: &captureRecorder{w: newCaptureWriter()}}
	if n, _ := io.Copy(io.Discard, tee); n != int64(database.PluginCaptureBodyLimit+10) {
		t.Fatalf("tee altered the stream: %d", n)
	}
	got := <-tee.rec.w.queue
	if !got.Truncated || len(got.Body) != database.PluginCaptureBodyLimit {
		t.Fatalf("tee capture truncated=%v len=%d", got.Truncated, len(got.Body))
	}
	tee.Close()
	select {
	case extra := <-tee.rec.w.queue:
		t.Fatalf("capture emitted twice: %+v", extra)
	default:
	}
}

func TestCaptureRetentionPurge(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	err = db.InsertPluginCaptures(context.Background(), []database.PluginCapture{
		{Plugin: "retplug", Direction: "request", CreatedAt: now.Add(-PluginCaptureRetention - time.Hour)},
		{Plugin: "retplug", Direction: "request", CreatedAt: now.Add(-PluginCaptureRetention + time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := PurgeExpiredCaptures(context.Background(), db, now)
	if err != nil || result.Deleted != 1 {
		t.Fatalf("purge = %+v, %v", result, err)
	}
	page, err := db.ListPluginCaptures(context.Background(), database.PluginCaptureFilter{Plugin: "retplug"})
	if err != nil || page.Total != 1 {
		t.Fatalf("remaining = %+v, %v", page, err)
	}
}
