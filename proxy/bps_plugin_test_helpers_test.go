package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// updateBPSConfig applies update to the active BPS plugin config for a test
// and restores the config the test started with.
func updateBPSConfig(t *testing.T, update func(BPSConfig) BPSConfig) BPSConfig {
	t.Helper()
	previous := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previous) })
	storeBPSConfig(update(currentBPSConfig()))
	return currentBPSConfig()
}

// withBPSOverride sets the BPS plugin's per-account override, as the
// codex_bps_enabled credential would.
func withBPSOverride(account *auth.Account, enabled bool) *auth.Account {
	account.SetTransportPluginOverride(BPSPluginID, enabled)
	return account
}

const accountIdentitySampleAccount = "661373c1-f1a9-4ca9-8682-a0594b30c36c"

// WithCodexIdentityStore attaches the durable BPS identity store (fj-server
// test helper name kept so the ported tests read unchanged).
func WithCodexIdentityStore(ctx context.Context, store any) context.Context {
	if BPSDiagnosticFromContext(ctx) == nil {
		ctx, _ = WithBPSDiagnosticRecorder(ctx)
	}
	if store == nil {
		return ctx
	}
	return context.WithValue(ctx, bpsIdentityStoreKey{}, store)
}

// bpsTraceTransport replaces fj's bpsTraceTransport(ctx) for
// the BPS part of the transport diagnostic.
func bpsTraceTransport(ctx context.Context) *bpsTestTransport {
	return &bpsTestTransport{BPS: BPSDiagnosticFromContext(ctx)}
}

type bpsTestTransport struct{ BPS *CodexBPSDiagnostic }

// WithCodexAccountTestIdentityStore mirrors fj's administrator-test helper:
// a stable per-account caller owner plus the identity store.
func WithCodexAccountTestIdentityStore(ctx context.Context, store any, account *auth.Account) context.Context {
	if account != nil && account.ID() > 0 {
		ctx = withBPSTestUser(ctx, fmt.Sprintf("account-test:%d", account.ID()))
	}
	return WithCodexIdentityStore(ctx, store)
}

// withBPSTestUser sets the verified caller owner BPS partitions by.
func withBPSTestUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, bpsCallerContextKey{}, bpsCaller{user: user})
}

// withBPSFullConvergenceTest adapts fj's call shape (headers, fingerprint).
func withBPSFullConvergenceTest(ctx context.Context, account *auth.Account, profile bpsProfileConfig, headers http.Header, _ any, cacheKey, apiKey string) (context.Context, error) {
	return withBPSFullConvergence(ctx, account, profile, headers, cacheKey, apiKey)
}

// Fixtures ported from fj-server's session and identity tests.
const (
	testRootSessionA          = "01a031a2-043b-7f42-afa6-ce5491d9be64"
	testLeafSessionA          = "01a031a2-ca1e-7063-8ba7-f140c182c629"
	accountIdentitySampleRoot = "01a09302-49f4-7b53-b545-91ef29610317"
	accountIdentitySampleCtx  = "01a09302-49f4-7b53-b545-91fb553a57b4"
)

func nativeSessionHeaders(root, leaf string, sequence int) http.Header {
	headers := http.Header{}
	headers.Set("Session-Id", root)
	headers.Set("Thread-Id", leaf)
	headers.Set("X-Client-Request-Id", leaf)
	headers.Set("X-Codex-Window-Id", leaf+":"+strconv.Itoa(sequence))
	if root != leaf {
		headers.Set("X-Codex-Parent-Thread-Id", root)
	}
	return headers
}

func accountIdentityFixture(test *testing.T, child bool, object bool) (http.Header, []byte) {
	test.Helper()
	thread := accountIdentitySampleRoot
	metadata := map[string]any{
		"session_id": accountIdentitySampleRoot, "thread_id": thread,
		"context_window_id": accountIdentitySampleCtx, "window_id": thread + ":0", "window_number": 0,
		"turn_id": "01a0939f-d89c-77f1-94fa-080df9ebda48", "root_turn_id": "01a0939f-d89c-77f1-94fa-080df9ebda48",
		"turn_started_at_unix_ms": int64(1789183121593), "thread_source": "user", "request_kind": "turn",
		"installation_id": "9dcfc09b-4e8b-4e25-9052-fefb87224807",
	}
	if child {
		thread = "01a09303-49f4-7b53-b545-920f29610317"
		metadata["thread_id"], metadata["window_id"] = thread, thread+":2"
		metadata["window_number"] = 2
		metadata["parent_thread_id"], metadata["forked_from_thread_id"] = accountIdentitySampleRoot, accountIdentitySampleRoot
		metadata["thread_source"], metadata["subagent_kind"] = "guardian_review", "guardian"
	}
	raw, err := json.Marshal(metadata)
	require.NoError(test, err)
	var embedded any = string(raw)
	if object {
		embedded = metadata
	}
	client := map[string]any{"session_id": accountIdentitySampleRoot, "thread_id": thread, "x-client-request-id": thread, "x-codex-window-id": metadata["window_id"], "x-codex-installation-id": metadata["installation_id"], "x-codex-turn-metadata": embedded}
	if child {
		client["parent_thread_id"], client["x-codex-forked-from-thread-id"] = accountIdentitySampleRoot, accountIdentitySampleRoot
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "client_metadata": client, "prompt_cache_key": "original-cache", "input": []any{map[string]any{"type": "compaction", "id": "opaque-id", "encrypted_content": "private-encrypted"}}})
	require.NoError(test, err)
	headers := http.Header{}
	headers.Set("Session-Id", accountIdentitySampleRoot)
	headers.Set("Thread-Id", thread)
	headers.Set("X-Client-Request-Id", thread)
	headers.Set(codexTurnMetadataHeader, string(raw))
	headers.Set("X-NewAPI-Meta", "signed-original-do-not-rewrite")
	return CodexRequestMetadataHeaders(headers, body), body
}

func transportTestContext() *gin.Context {
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attachUpstreamTrace(request, nil)
	ctx, _ := WithBPSDiagnosticRecorder(request.Request.Context())
	request.Request = request.Request.WithContext(ctx)
	return request
}

// executeBPSTestRequest / executeBPSTestCompact stand in for fj-server's
// executor entry points, which routed BPS accounts internally: accounts the
// BPS plugin serves go through the plugin route (Execute plus the response
// transformer, as in production), all others through the native executor.
func executeBPSTestRequest(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, error) {
	if route := resolveBPSTestRoute(ctx, account, body, headers, plugins.KindResponses); route != nil {
		return route.Execute(ctx, plugins.ReqEnv{Account: account, Model: gjson.GetBytes(body, "model").String(), Body: body, Header: headers, CacheKey: sessionID, ProxyURL: proxyOverride, APIKey: apiKey})
	}
	return ExecuteRequest(ctx, account, body, sessionID, proxyOverride, apiKey, deviceCfg, headers, useWebsocket...)
}

func executeBPSTestCompact(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header) (*http.Response, error) {
	if route := resolveBPSTestRoute(ctx, account, body, headers, plugins.KindResponsesCompact); route != nil {
		return route.Execute(ctx, plugins.ReqEnv{Account: account, Model: gjson.GetBytes(body, "model").String(), Body: body, Header: headers, CacheKey: sessionID, ProxyURL: proxyOverride, APIKey: apiKey, Compact: true})
	}
	return ExecuteCompactRequest(ctx, account, body, sessionID, proxyOverride, apiKey, deviceCfg, headers)
}

func resolveBPSTestRoute(ctx context.Context, account *auth.Account, body []byte, headers http.Header, kind plugins.RequestKind) *plugins.Route {
	if account == nil || account.IsRelayStyle() {
		return nil
	}
	req := plugins.NewRequest(NewUpstreamSessionUUID(), kind, body, headers, 0)
	return plugins.Default().Resolve(ctx, req, account, gjson.GetBytes(body, "model").String(), kind)
}

// bindInferredBPSSessionTest keeps fj's shape: the inferred session is bound
// to the request context instead of the plugin's request state.
func bindInferredBPSSessionTest(c *gin.Context, body []byte, identity requestSessionIdentity, root requestRootSessionIdentity) {
	state := bindInferredBPSSession(c, body, identity, root)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), inferredBPSSessionKey{}, state))
}

func bindBPSUploadRequestTest(h *Handler, c *gin.Context, body []byte, compact bool) {
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), bpsUploadRequestKey{}, h.newBPSUploadRequest(c, body, compact)))
}

// bpsUploadCooldownForRequest is fj's route-aware check: only the BPS route
// is subject to BPS upload cooldowns.
func bpsUploadCooldownForRequest(ctx context.Context, account *auth.Account, mode string) bool {
	return mode == "bps" && bpsUploadCooldownForAccount(ctx, account)
}

func freshRetryImage(t *testing.T) string {
	data, err := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, err)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(data, []byte(t.Name())...))
}

// bpsTransformTestBody runs a BPS response through the plugin's response
// transformer (the seam that replaced fj's turn-state stream wrapper).
func bpsTransformTestBody(ctx context.Context, body string, stream bool) ([]byte, error) {
	env := &plugins.ReqEnv{}
	env.SetState(bpsAttemptDiagnosticKey, bpsDiagnosticFromContext(ctx))
	header := http.Header{"Content-Type": {"application/json"}}
	if stream {
		header.Set("Content-Type", "text/event-stream")
	}
	resp := &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body))}
	if err := plugins.ApplyResponseTransformer(resp, bpsPlugin{}, env); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// newBPSProxyTestDB opens a database with the BPS plugin tables, which the
// core migration no longer creates.
func newBPSProxyTestDB(driver, dsn string) (*database.DB, error) {
	db, err := database.New(driver, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.MigrateBPSPlugin(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// enableBPSAttachmentFallbackForTest turns the (default OFF) 429 fallback on
// and pretends the converter binaries are installed.
func enableBPSAttachmentFallbackForTest(t *testing.T) {
	t.Helper()
	previous := currentBPSConfig()
	storeBPSConfig(BPSConfig{Attachment429Fallback: true})
	bpsConverterProbe.once.Do(func() {})
	installed := bpsConverterProbe.installed
	bpsConverterProbe.installed = true
	t.Cleanup(func() {
		storeBPSConfig(previous)
		bpsConverterProbe.installed = installed
	})
}
