package wsrelay

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	appconfig "github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestForkAccountFallbackDispatchToWebsocket(t *testing.T) {
	oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(oldRuntime)
		proxy.SetResinConfig(oldResin)
		proxy.WebsocketExecuteFunc = oldExecutor
	})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexForceWebsocket, settings.CodexForkAccountFallbackEnabled = true, true
	proxy.ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "fork.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	memory := cache.NewMemory(100)
	t.Cleanup(func() { require.NoError(t, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	t.Cleanup(store.Stop)
	for i := 0; i < 2; i++ {
		store.AddAccount(&auth.Account{DBID: int64(1695 + i), AccountID: fmt.Sprintf("%d61373c1-f1a9-4ca9-8682-a0594b30c36c", 6+i), AccessToken: fmt.Sprintf("token-%d", i), Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice, SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 60})
	}
	config := store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = "enforce"
	store.SetPromptFilterConfig(config)
	handler := proxy.NewHandler(store, db, &appconfig.Config{AllowAnonymousV1: true}, nil)
	handler.SetRuntimeCache(memory)
	router := gin.New()
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), handler.Responses)
	router.GET("/v1/responses", handler.APIKeyAuthMiddleware(), handler.ResponsesWebSocket)
	type capture struct {
		headers http.Header
		body    []byte
	}
	seen := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			seen <- capture{r.Header.Clone(), body}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"fork-response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "fork-ws"})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)
	parent, child := proxy.NewUpstreamSessionUUID(), proxy.NewUpstreamSessionUUID()
	var parentAccount, childAccount, childSession string
	for i := 0; i < 3; i++ {
		root, number := child, 27
		if i == 0 {
			root, number = parent, 0
		}
		body := []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"input":"full plaintext fork context","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","thread_source":"user","request_kind":"turn","window_id":"%s:%d","window_number":%d}}}`, root, root, root, root, root, number, number))
		if i > 0 {
			body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.forked_from_thread_id", parent)
			require.NoError(t, err)
		}
		body = addSessionWireTools(t, body)
		if i < 2 {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer test-user-key")
			ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
			router.ServeHTTP(recorder, request.WithContext(ctx))
			cancel()
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "response.completed")
		} else {
			// The established child keeps its own owner through a WS reconnect,
			// even with initial-fork fallback disabled and parent refs resent.
			settings.CodexForkAccountFallbackEnabled = false
			proxy.ApplyRuntimeSettings(settings)
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-user-key"}})
			require.NoError(t, err)
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
			_, result, err := conn.ReadMessage()
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(result, "type").String(), string(result))
			require.NoError(t, conn.Close())
		}
		select {
		case sent := <-seen:
			assertSessionWireTools(t, sent.body)
			meta := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
			require.False(t, meta.Get("forked_from_thread_id").Exists())
			require.Zero(t, meta.Get("window_number").Uint())
			if i == 0 {
				parentAccount = sent.headers.Get("Chatgpt-Account-Id")
			} else if i == 1 {
				childAccount, childSession = sent.headers.Get("Chatgpt-Account-Id"), sent.headers.Get("Session-Id")
				require.NotEqual(t, parentAccount, childAccount)
			} else {
				require.Equal(t, childAccount, sent.headers.Get("Chatgpt-Account-Id"))
				require.Equal(t, childSession, sent.headers.Get("Session-Id"))
			}
		case <-time.After(time.Second):
			t.Fatal("upstream frame missing")
		}
	}
}

func TestRelaxedPassiveDispatchToWebsocket(t *testing.T) {
	oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(oldRuntime)
		proxy.SetResinConfig(oldResin)
		proxy.WebsocketExecuteFunc = oldExecutor
	})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexForceWebsocket, settings.CodexForkAccountFallbackEnabled = true, true
	proxy.ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "relaxed.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	memory := cache.NewMemory(100)
	t.Cleanup(func() { require.NoError(t, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	t.Cleanup(store.Stop)
	for i := 0; i < 1; i++ {
		store.AddAccount(&auth.Account{DBID: int64(1695 + i), AccountID: fmt.Sprintf("%d61373c1-f1a9-4ca9-8682-a0594b30c36c", 6+i), AccessToken: fmt.Sprintf("token-%d", i), Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice, SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 60})
	}
	config := store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = "enforce"
	store.SetPromptFilterConfig(config)
	handler := proxy.NewHandler(store, db, &appconfig.Config{AllowAnonymousV1: true}, nil)
	handler.SetRuntimeCache(memory)
	router := gin.New()
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), handler.Responses)
	router.GET("/v1/responses", handler.APIKeyAuthMiddleware(), handler.ResponsesWebSocket)
	type capture struct {
		headers http.Header
		body    []byte
	}
	seen := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			seen <- capture{r.Header.Clone(), body}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"fork-response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "fork-ws"})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)

	parent, leaf := proxy.NewUpstreamSessionUUID(), proxy.NewUpstreamSessionUUID()
	var previousSession string
	var downstreamConn *websocket.Conn
	for i := 0; i < 3; i++ {
		body := []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"input":"full plaintext passive context","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","parent_thread_id":"%s","thread_source":"thread_title","request_kind":"turn","window_id":"%s:27","window_number":27}}}`, parent, leaf, parent, leaf, parent, leaf))
		body = addSessionWireTools(t, body)
		if i == 0 {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer test-user-key")
			ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
			router.ServeHTTP(recorder, request.WithContext(ctx))
			cancel()
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "response.completed")
		} else {
			if downstreamConn == nil {
				downstreamConn, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-user-key"}})
				require.NoError(t, err)
				t.Cleanup(func() { downstreamConn.Close() })
			}
			require.NoError(t, downstreamConn.SetReadDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, downstreamConn.WriteMessage(websocket.TextMessage, body))
			_, result, err := downstreamConn.ReadMessage()
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(result, "type").String(), string(result))
		}
		select {
		case sent := <-seen:
			assertSessionWireTools(t, sent.body)
			meta := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
			require.False(t, meta.Get("parent_thread_id").Exists(), string(sent.body))
			require.Zero(t, meta.Get("window_number").Uint())
			session := sent.headers.Get("Session-Id")
			require.NotEmpty(t, session)
			require.NotEqual(t, previousSession, session, "each temporary request owns a separate outbound segment")
			previousSession = session
		case <-time.After(time.Second):
			t.Fatal("upstream frame missing")
		}
	}
	require.Eventually(t, func() bool { count, _ := store.AccountSessionSlotCounts(1695, time.Now()); return count == 0 }, time.Second, time.Millisecond)
}
