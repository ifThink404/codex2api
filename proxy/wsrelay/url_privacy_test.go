package wsrelay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestURLPrivacyAtActualTransportBoundary(t *testing.T) {
	for _, transport := range []string{"http", "ws", "compact", "relay", "relay_compact"} {
		for _, source := range []string{"user", "subagent", "memory_consolidation", "system", "guardian_review", "guardian_classifier"} {
			t.Run(transport+"/"+source, func(t *testing.T) {
				t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
				t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
				oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
				t.Cleanup(func() {
					proxy.ApplyRuntimeSettings(oldRuntime)
					proxy.SetResinConfig(oldResin)
					proxy.WebsocketExecuteFunc = oldExecutor
				})
				proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
				const original = "https://customer-gateway.example/v1"
				target := "https://chatgpt.com/backend-api/codex"
				if strings.HasPrefix(transport, "relay") {
					target = "https://api.openai.com/v1"
				}
				type capture struct {
					body    []byte
					headers http.Header
					path    string
				}
				seen := make(chan capture, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reply := map[string]any{"id": "resp_url_test", "status": "completed", "metadata": map[string]string{"base_url": target}, "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": target}}}}}
					if websocket.IsWebSocketUpgrade(r) {
						connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer connection.Close()
						_, body, err := connection.ReadMessage()
						if err != nil {
							return
						}
						seen <- capture{body, r.Header.Clone(), r.URL.Path}
						_ = connection.WriteJSON(map[string]any{"type": "response.completed", "response": reply})
						return
					}
					body, _ := io.ReadAll(r.Body)
					seen <- capture{body, r.Header.Clone(), r.URL.Path}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(reply)
				}))
				t.Cleanup(server.Close)
				proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "url-privacy-test"})
				db, err := database.New("sqlite", filepath.Join(t.TempDir(), "url.db"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 10*time.Second)
				defer cancel()
				account := &auth.Account{DBID: 42, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "test-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice, CustomHeaders: map[string]string{"X-Copy-Url": original + "/responses", "X-Unrelated": "https://ordinary.example"}}
				if strings.HasPrefix(transport, "relay") {
					account.UpstreamType = auth.UpstreamOpenAIResponses
					account.BaseURL = server.URL
					account.APIKey = "test-key"
				}
				manager := NewManager()
				t.Cleanup(manager.Stop)
				proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, override, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
					result, err := NewExecutorWithManager(manager).ExecuteRequestViaWebsocket(ctx, a, body, session, override, key, config, headers, pool)
					if err != nil {
						return nil, err
					}
					return websocketResponseToHTTP(ctx, result, http.StatusOK, nil), nil
				}
				const root = "01a09302-49f4-7b53-b545-91ef29610317"
				meta, _ := json.Marshal(map[string]any{"session_id": root, "thread_id": root, "window_id": root + ":0", "thread_source": source, "request_kind": "turn", "base_url": original, "openai_base_url": original, "note": "使用 " + original, "encoded": `{"items":[{"url":"` + original + `/responses"}]}`})
				cm := map[string]string{"session_id": root, "thread_id": root, "base_url": original, "x-codex-turn-metadata": string(meta)}
				body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]string{"role": "user", "content": original}}, "client_metadata": cm, "metadata": map[string]string{"base_url": original, "note": "使用 " + original}, "prompt_cache_key": root})
				headers := http.Header{"Session-Id": {root}, "Thread-Id": {root}, "X-Codex-Turn-Metadata": {string(meta)}}
				var response *http.Response
				switch transport {
				case "compact":
					response, err = proxy.ExecuteCompactRequest(ctx, account, body, root, "", "caller", nil, headers)
				case "relay":
					response, err = proxy.ExecuteOpenAIResponsesRequest(ctx, account, body, "", headers)
				case "relay_compact":
					response, err = proxy.ExecuteOpenAIResponsesCompactRequest(ctx, account, body, "", headers)
				default:
					response, err = proxy.ExecuteRequest(ctx, account, body, root, "", "caller", nil, headers, transport == "ws")
				}
				require.NoError(t, err)
				returned, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Contains(t, string(returned), `"base_url":"`+original+`"`)
				require.Contains(t, string(returned), `"text":"`+target+`"`, "business response URLs must not be restored")
				wire := <-seen
				require.Contains(t, wire.path, "/responses")
				require.Equal(t, target+"/responses", wire.headers.Get("X-Copy-Url"))
				require.Equal(t, "https://ordinary.example", wire.headers.Get("X-Unrelated"))
				require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(wire.body, "input").Raw)
				if !strings.Contains(transport, "compact") {
					outMeta := gjson.Parse(gjson.GetBytes(wire.body, "client_metadata.x-codex-turn-metadata").String())
					require.Equal(t, target, outMeta.Get("base_url").String())
					require.Equal(t, target, outMeta.Get("openai_base_url").String())
					require.Equal(t, target, gjson.GetBytes(wire.body, "client_metadata.base_url").String())
					require.Equal(t, "使用 "+target, outMeta.Get("note").String())
					require.NotContains(t, outMeta.Raw, original)
				}
				if transport == "relay" {
					require.Equal(t, target, gjson.GetBytes(wire.body, "metadata.base_url").String())
				}
				require.NotContains(t, wire.headers.Get("X-Codex-Turn-Metadata"), original)
				if transport == "ws" {
					require.False(t, gjson.Get(wire.headers.Get("X-Codex-Turn-Metadata"), "base_url").Exists())
				}
			})
		}
	}
}
