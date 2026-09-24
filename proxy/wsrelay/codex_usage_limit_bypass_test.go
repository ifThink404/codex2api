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
)

func TestUsageLimitBypassHTTPAndWebsocket(t *testing.T) {
	oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(oldRuntime)
		proxy.SetResinConfig(oldResin)
		proxy.WebsocketExecuteFunc = oldExecutor
	})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexForceWebsocket, settings.CodexSessionFailoverEnabled = true, true
	proxy.ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "fork.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	memory := cache.NewMemory(100)
	t.Cleanup(func() { require.NoError(t, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "test", PlanType: "pro", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol", "gpt-6-astra"}, CodexUsageLimitBypassEnabled: true, CodexUsageLimitBypassModels: []string{"gpt-5.6-sol"}}
	store.AddAccount(account)
	account.SetUsageSnapshot5h(100, time.Now().Add(time.Hour))
	account.SetUsageSnapshot(100, time.Now())
	account.SetReset7dAt(time.Now().Add(24 * time.Hour))
	store.MarkUsage7dRateLimited(account)

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
	root := proxy.NewUpstreamSessionUUID()
	for i := 0; i < 5; i++ {
		model := "gpt-5.6-sol"
		if i == 3 {
			model = "gpt-6-astra"
			root = proxy.NewUpstreamSessionUUID()
		}
		if i == 4 {
			store.ApplyAccountUsageLimitBypass(account.ID(), database.OptionalBool{Set: true}, database.OptionalStringSlice{})
		}
		body := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"stream":true,"input":"hello","client_metadata":{"session_id":%q,"thread_id":%q,"x-codex-turn-metadata":{"session_id":%q,"thread_id":%q,"thread_source":"user","request_kind":"turn","window_id":%q,"window_number":0}}}`, model, root, root, root, root, root+":0"))
		if i == 2 {
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-user-key"}})
			require.NoError(t, err)
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
			_, result, err := conn.ReadMessage()
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(result, "type").String(), string(result))
			require.NoError(t, conn.Close())
		} else {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer test-user-key")
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			router.ServeHTTP(w, r.WithContext(ctx))
			cancel()
			if i < 3 {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "response.completed")
			} else {
				require.Equal(t, 429, w.Code, w.Body.String())
			}
		}
		if i < 3 {
			select {
			case sent := <-seen:
				require.Equal(t, account.AccountID, sent.headers.Get("Chatgpt-Account-Id"))
				require.Equal(t, model, gjson.GetBytes(sent.body, "model").String())
			case <-time.After(time.Second):
				t.Fatal("exempt model never reached upstream")
			}
		} else {
			select {
			case <-seen:
				t.Fatal("quota-limited model reached upstream")
			default:
			}
		}
	}
}
