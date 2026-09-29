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

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// bpsHandlerFixture is a gateway with the BPS plugin attached to a real
// database, one native Codex account and a scripted upstream for it.
type bpsHandlerFixture struct {
	handler  *Handler
	db       *database.DB
	store    *auth.Store
	registry *plugins.Registry
	account  *auth.Account
	native   atomic.Int32
	bps      atomic.Int32
	uploads  atomic.Int32
	lastBody atomic.Pointer[[]byte]
	upload   func() (int, string)
}

const bpsFixtureCompleted = `{"type":"response.completed","response":{"id":"resp_bps_fixture","object":"response","instructions":"Basis Points internal runtime","metadata":{"bps_tools_version_id":"private"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":14000,"output_tokens":5,"total_tokens":14005,"input_tokens_details":{"cached_tokens":0}}}}`

func newBPSHandlerFixture(t *testing.T, credentials map[string]any) *bpsHandlerFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	f := &bpsHandlerFixture{upload: func() (int, string) { return 200, `{"openai_file_id":"file-fixture"}` }}
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "bps-handler.db"))
	require.NoError(t, err)
	f.db = db
	db.SetUsageLogConfig(database.UsageLogModeFull, 100, 300)

	f.registry = plugins.NewRegistry()
	f.registry.Register(bpsPlugin{})
	require.NoError(t, f.registry.Attach(context.Background(), db))
	previous := plugins.SwapDefault(f.registry)
	previousConfig := currentBPSConfig()

	f.store = auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, MaxRetries: 1})
	t.Cleanup(func() {
		f.store.Stop()
		plugins.SwapDefault(previous)
		auth.SetTransportPluginReloader(nil)
		storeBPSConfig(previousConfig)
		_ = db.Close()
	})
	creds := map[string]any{"access_token": "at-fixture", "account_id": "acct-fixture", "refresh_token": "rt-fixture", "plan_type": "pro"}
	for k, v := range credentials {
		creds[k] = v
	}
	id, err := db.InsertAccountWithCredentials(context.Background(), "bps-fixture", creds, "")
	require.NoError(t, err)
	require.NoError(t, f.store.Init(context.Background()))
	f.account = f.store.FindByID(id)
	require.NotNil(t, f.account)
	f.account.AccessToken = "at-fixture"
	f.account.AccountID = "acct-fixture"
	f.account.Status = auth.StatusReady

	f.handler = NewHandler(f.store, db, &config.Config{AllowAnonymousV1: true}, nil)
	f.handler.SetRuntimeCache(cache.NewMemory(1))
	installClaudeBoundaryTransport(t, f.account, func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/attachments"):
			f.uploads.Add(1)
			status, data := f.upload()
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(data)), Request: r}, nil
		case strings.HasPrefix(r.URL.String(), CodexBPSBaseURL):
			f.bps.Add(1)
			f.lastBody.Store(&body)
		default:
			f.native.Add(1)
		}
		sse := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"OK from Basis Po\"}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"ints\"}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}}\n\n" +
			"data: " + bpsFixtureCompleted + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse)), Request: r}, nil
	})
	return f
}

func (f *bpsHandlerFixture) serve(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	attachUpstreamTrace(ctx, f.store)
	switch path {
	case "/v1/chat/completions":
		f.handler.ChatCompletions(ctx)
	default:
		f.handler.Responses(ctx)
	}
	return recorder
}

func (f *bpsHandlerFixture) usageRows(t *testing.T) []*database.UsageLog {
	t.Helper()
	f.db.FlushUsageLogs()
	logs, err := f.db.ListRecentUsageLogs(context.Background(), 50)
	require.NoError(t, err)
	return logs
}

// codex_bps_enabled rows written before the plugin (true) keep routing to
// BPS: the credential key is the plugin's per-account override.
func TestBPSPluginServesLegacyEnabledAccountThroughResponses(t *testing.T) {
	f := newBPSHandlerFixture(t, map[string]any{auth.CodexBPSEnabledCredentialKey: true})
	recorder := f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"input":"hi","instructions":"Caller instructions"}`)
	body := recorder.Body.String()
	require.Equal(t, http.StatusOK, recorder.Code, body)
	require.EqualValues(t, 1, f.bps.Load())
	require.Zero(t, f.native.Load())
	require.NotContains(t, body, "Basis Points")
	require.NotContains(t, body, "Basis Po", "a provider name split across deltas is redacted too")
	require.NotContains(t, body, "bps_tools_version_id")
	require.Contains(t, body, "Caller instructions", "response instructions project back to the caller's")
	sent := *f.lastBody.Load()
	require.Equal(t, "word", gjson.GetBytes(sent, "metadata.bps_tools_version_id").String()[len("tools-"):len("tools-word")])
	require.False(t, gjson.GetBytes(sent, "instructions").Exists())

	rows := f.usageRows(t)
	require.Len(t, rows, 1)
	row := rows[0]
	require.Equal(t, BPSPluginID, row.Transport)
	require.Equal(t, CodexBPSBaseURL+"/responses", row.UpstreamEndpoint)
	require.False(t, row.ViaWebsocket)
	require.Equal(t, "word", gjson.Get(row.PluginMeta, "profile").String())
	require.Equal(t, "1", gjson.Get(row.PluginMeta, "agent_iteration").String())
	// Fixed runtime-overhead billing ran before usage extraction.
	require.Equal(t, 14000-int(bpsFixedInputOverhead(auth.BPSWord)), row.InputTokens)
}

func TestBPSPluginDefaultOffGroupAndGlobalEnablement(t *testing.T) {
	f := newBPSHandlerFixture(t, nil)
	require.Equal(t, http.StatusOK, f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`).Code)
	require.Zero(t, f.bps.Load(), "BPS is off by default")
	require.EqualValues(t, 1, f.native.Load())

	f.store.ApplyAccountGroups(f.account.ID(), []int64{41})
	require.NoError(t, f.registry.Save(context.Background(), database.TransportPluginState{ID: BPSPluginID, GroupIDs: []int64{41}}))
	require.Equal(t, http.StatusOK, f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`).Code)
	require.EqualValues(t, 1, f.bps.Load(), "account group enables BPS")

	off := false
	f.store.ApplyAccountTransportPluginOverride(f.account.ID(), BPSPluginID, &off)
	require.NoError(t, f.registry.Save(context.Background(), database.TransportPluginState{ID: BPSPluginID, Enabled: true}))
	require.Equal(t, http.StatusOK, f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`).Code)
	require.EqualValues(t, 1, f.bps.Load(), "an explicit override wins over the global switch")
	require.EqualValues(t, 2, f.native.Load())
}

// Upstream's openai_excel_bps flag means "BPS plugin, Excel profile"; the
// upstream adapter branch never runs.
func TestBPSPluginTakesOverUpstreamExcelFlag(t *testing.T) {
	f := newBPSHandlerFixture(t, map[string]any{auth.ExcelBPSCredentialKey: true})
	require.Equal(t, http.StatusOK, f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`).Code)
	require.EqualValues(t, 1, f.bps.Load())
	sent := *f.lastBody.Load()
	require.True(t, strings.HasPrefix(gjson.GetBytes(sent, "metadata.bps_tools_version_id").String(), "tools-excel-"))
	rows := f.usageRows(t)
	require.Equal(t, "excel", gjson.Get(rows[0].PluginMeta, "profile").String())
}

// Continuations of BPS-produced responses are pinned to BPS.
func TestBPSPluginPinsContinuationsOfBPSResponses(t *testing.T) {
	f := newBPSHandlerFixture(t, map[string]any{auth.CodexBPSEnabledCredentialKey: true})
	require.Equal(t, http.StatusOK, f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`).Code)
	off := false
	f.store.ApplyAccountTransportPluginOverride(f.account.ID(), BPSPluginID, &off)
	recorder := f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_bps_fixture","input":"next"}`)
	require.NotEqual(t, http.StatusOK, recorder.Code, "a BPS continuation cannot fall back to native")
	require.Zero(t, f.native.Load())
	// An unrelated continuation is unaffected.
	require.Equal(t, http.StatusOK, f.serve(t, "/v1/responses", `{"model":"gpt-5.6-sol","stream":true,"input":"fresh"}`).Code)
	require.EqualValues(t, 1, f.native.Load())
}

// An upload 429 is one zero-token attempt and starts the account's upload
// cooldown, which then vetoes the account for inline attachments.
func TestBPSPluginUploadFailureLogsAttemptAndCoolsDown(t *testing.T) {
	f := newBPSHandlerFixture(t, map[string]any{auth.CodexBPSEnabledCredentialKey: true})
	f.upload = func() (int, string) { return 429, `{"error":{"message":"Rate limit exceeded"}}` }
	t.Cleanup(func() {
		bpsUploadCooldownState.mu.Lock()
		delete(bpsUploadCooldownState.entries, bpsUploadCooldownKey(f.account))
		bpsUploadCooldownState.mu.Unlock()
	})
	image := freshRetryImage(t)
	body := `{"model":"gpt-5.6-sol","stream":false,"input":[{"role":"user","content":[{"type":"input_image","image_url":"` + image + `"}]}]}`
	recorder := f.serve(t, "/v1/responses", body)
	require.NotEqual(t, http.StatusOK, recorder.Code)
	require.Zero(t, f.bps.Load(), "inference never runs without the attachment")
	rows := f.usageRows(t)
	require.NotEmpty(t, rows)
	require.Equal(t, "bps_attachment_upload", rows[0].UpstreamErrorKind)
	require.Equal(t, http.StatusTooManyRequests, rows[0].StatusCode)
	require.Equal(t, BPSPluginID, rows[0].Transport)
	require.Zero(t, rows[0].InputTokens)

	uploads := f.uploads.Load()
	f.serve(t, "/v1/responses", body)
	require.Equal(t, uploads, f.uploads.Load(), "the cooling account is not selected for another upload")
}

func TestBPSPluginServesChatCompletions(t *testing.T) {
	f := newBPSHandlerFixture(t, map[string]any{auth.CodexBPSEnabledCredentialKey: true})
	recorder := f.serve(t, "/v1/chat/completions", `{"model":"gpt-5.6-sol","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.EqualValues(t, 1, f.bps.Load())
	require.Zero(t, f.native.Load())
	require.Contains(t, recorder.Body.String(), "OK")
	rows := f.usageRows(t)
	require.Equal(t, BPSPluginID, rows[0].Transport)
}

func TestBPSPluginConfigValidation(t *testing.T) {
	cfg, err := parseBPSConfig([]byte(`{"round_convergence_limit":0,"turn_round_limit":5,"attachment_429_fallback":true}`))
	require.NoError(t, err)
	require.Equal(t, 100, cfg.RoundConvergenceLimit, "out-of-range values use the default")
	require.Equal(t, 5, cfg.TurnRoundLimit)
	require.True(t, cfg.Attachment429Fallback)
	_, err = parseBPSConfig([]byte(`{"unknown_key":1}`))
	require.Error(t, err)
	require.False(t, BPSConfig{}.normalized().Attachment429Fallback, "the 429 fallback is off by default")
}

func TestBPSAttachmentFallbackRequiresOptInAndBinaries(t *testing.T) {
	previous := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previous) })
	bpsConverterProbe.once.Do(func() {})
	installed := bpsConverterProbe.installed
	t.Cleanup(func() { bpsConverterProbe.installed = installed })

	t.Setenv("CODEX_BPS_ATTACHMENT_429_FALLBACK", "")
	storeBPSConfig(BPSConfig{})
	bpsConverterProbe.installed = true
	require.False(t, bpsAttachmentFallbackEnabled(), "off by default")
	storeBPSConfig(BPSConfig{Attachment429Fallback: true})
	require.True(t, bpsAttachmentFallbackEnabled())
	bpsConverterProbe.installed = false
	require.False(t, bpsAttachmentFallbackEnabled(), "never active without the converter binaries")
}

// The connection-test hook follows the account setting in auto mode and an
// explicit mode otherwise, without changing the account.
func TestExecuteCodexConnectionTestModes(t *testing.T) {
	f := newBPSHandlerFixture(t, nil)
	payload := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`)
	run := func(mode string) {
		t.Helper()
		ctx, err := WithCodexTestMode(context.Background(), mode)
		require.NoError(t, err)
		resp, err := ExecuteCodexConnectionTest(ctx, f.account, payload, "")
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	run("auto")
	require.EqualValues(t, 1, f.native.Load(), "BPS is off for this account")
	run("bps")
	require.EqualValues(t, 1, f.bps.Load(), "explicit BPS test mode reaches BPS")
	_, overridden := f.account.TransportPluginOverride(BPSPluginID)
	require.False(t, overridden, "a test mode never writes the account")
	on := true
	f.store.ApplyAccountTransportPluginOverride(f.account.ID(), BPSPluginID, &on)
	run("auto")
	require.EqualValues(t, 2, f.bps.Load())
	// Native (codex) mode is refused for an account BPS owns...
	ctx, err := WithCodexTestMode(context.Background(), "codex")
	require.NoError(t, err)
	require.ErrorContains(t, ValidateCodexTestMode(ctx, f.account), "未开启原生路由")
	_, err = ExecuteCodexConnectionTest(ctx, f.account, payload, "")
	var refusal *Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusBadRequest, refusal.HTTPStatus)
	require.EqualValues(t, 1, f.native.Load(), "no native request was sent")
	// ...and allowed once its native route is explicitly enabled.
	native := true
	f.account.SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: &native})
	require.NoError(t, ValidateCodexTestMode(ctx, f.account))
	run("codex")
	require.EqualValues(t, 2, f.native.Load(), "explicit Codex mode stays native")
}

func TestConnectionTestUsageRecordsThePluginTransport(t *testing.T) {
	f := newBPSHandlerFixture(t, nil)
	payload := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`)
	test := func() database.UsageLogInput {
		t.Helper()
		ctx, usage := WithConnectionTestUsage(context.Background())
		resp, err := ExecuteCodexConnectionTest(ctx, f.account, payload, "")
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		input := database.UsageLogInput{Endpoint: "/v1/responses", UpstreamEndpoint: "/v1/responses", InternalReason: "connection_test"}
		usage.Apply(&input)
		return input
	}
	native := test()
	require.EqualValues(t, 1, f.native.Load())
	require.Empty(t, native.Transport, "a native test keeps the default transport")
	require.Equal(t, "/v1/responses", native.UpstreamEndpoint)

	on := true
	f.store.ApplyAccountTransportPluginOverride(f.account.ID(), BPSPluginID, &on)
	served := test()
	require.EqualValues(t, 1, f.bps.Load())
	require.Equal(t, BPSPluginID, served.Transport)
	require.Equal(t, CodexBPSBaseURL+"/responses", served.UpstreamEndpoint)
	require.Equal(t, "word", gjson.Get(served.PluginMeta, "profile").String())

	// A nil or unused usage handle is a no-op.
	var none *ConnectionTestUsage
	none.Apply(&served)
	_, unused := WithConnectionTestUsage(context.Background())
	untouched := database.UsageLogInput{UpstreamEndpoint: "/v1/responses"}
	unused.Apply(&untouched)
	require.Empty(t, untouched.Transport)
}
