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
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestProjectIdentityRealWebsocketRoundTrip(t *testing.T) {
	const original = "01a07f21-24a6-7ee2-b095-f0f5fdaee3d3"
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
			t.Cleanup(func() {
				proxy.ApplyRuntimeSettings(oldRuntime)
				proxy.SetResinConfig(oldResin)
				proxy.WebsocketExecuteFunc = oldExecutor
			})
			settings := proxy.DefaultRuntimeSettings()
			settings.CodexSessionFailoverEnabled = true
			settings.CodexForceWebsocket = true
			settings.CodexWSSilentRetry = false
			settings.CodexWSSilentRetries = 0
			proxy.ApplyRuntimeSettings(settings)
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "projects.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			memory := cache.NewMemory(100)
			t.Cleanup(func() { _ = memory.Close() })
			store := auth.NewStore(nil, memory, nil)
			store.SetMaxRetries(0)
			store.SetMaxRateLimitRetries(0)
			t.Cleanup(store.Stop)
			store.AddAccount(&auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "fake-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice})
			handler := proxy.NewHandler(store, db, nil, nil)
			handler.SetRuntimeCache(memory)
			type projectWire struct {
				body    []byte
				headers http.Header
			}
			captured := make(chan projectWire, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if e != nil {
					return
				}
				defer conn.Close()
				for n := 0; n < 2; n++ {
					_, body, e := conn.ReadMessage()
					if e != nil {
						return
					}
					captured <- projectWire{body, r.Header.Clone()}
					alias := ""
					for _, item := range gjson.GetBytes(body, "input").Array() {
						if item.Get("role").String() == "user" {
							for _, part := range item.Get("content").Array() {
								if text := part.Get("text"); text.Exists() {
									alias = text.String()
								}
							}
						}
					}
					item := map[string]any{"id": fmt.Sprintf("msg_project_%d", n), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": alias}}}
					id := item["id"].(string)
					mid := len(alias) / 2
					for _, event := range []any{
						map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("resp_project_%d", n), "output": []any{}}},
						map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": id, "type": "message", "role": "assistant", "content": []any{}}},
						map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": 0, "content_index": 0, "delta": alias[:mid]},
						map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": 0, "content_index": 0, "delta": alias[mid:]},
						map[string]any{"type": "response.output_text.done", "item_id": id, "output_index": 0, "content_index": 0, "text": alias},
						map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
						map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_project_%d", n), "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 10}}},
					} {
						if e = conn.WriteJSON(event); e != nil {
							return
						}
					}
				}
			}))
			t.Cleanup(upstream.Close)
			proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "project-ws-test"})
			manager := NewManager()
			t.Cleanup(manager.Stop)
			executor := NewExecutorWithManager(manager)
			proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
				response, err := executor.ExecuteRequestViaWebsocket(ctx, a, body, session, proxyURL, key, config, headers, pool)
				if err != nil {
					return nil, err
				}
				return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
			}
			root := uuid.Must(uuid.NewV7()).String()
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":%q}]}],"client_metadata":{"project_id":%q,"session_id":%q,"thread_id":%q,"x-codex-turn-metadata":{"session_id":%q,"thread_id":%q,"thread_source":"user","request_kind":"turn","window_id":%q,"window_number":0}}}`, original, original, root, root, root, root, root+":0"))
			body = addSessionWireTools(t, body)
			body, _ = sjson.SetBytes(body, "client_metadata.workspace_id", original)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.project_id", original)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.workspace_id", original)
			var client *websocket.Conn
			if native {
				engine := gin.New()
				engine.GET("/v1/responses", handler.ResponsesWebSocket)
				server := httptest.NewServer(engine)
				t.Cleanup(server.Close)
				client, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer project-user-key"}, "X-Codex-Project-Id": []string{original}, "X-Codex-Workspace-Id": []string{original}})
				require.NoError(t, err)
				defer client.Close()
				body, _ = sjson.SetBytes(body, "type", "response.create")
			}
			var previousAlias string
			for turn := 0; turn < 2; turn++ {
				var output string
				if native {
					require.NoError(t, client.SetReadDeadline(time.Now().Add(10*time.Second)))
					require.NoError(t, client.WriteMessage(websocket.TextMessage, body))
					for {
						_, data, e := client.ReadMessage()
						require.NoError(t, e)
						output += string(data)
						kind := gjson.GetBytes(data, "type").String()
						require.NotEqual(t, "error", kind, string(data))
						if kind == "response.completed" || kind == "response.failed" {
							break
						}
					}
				} else {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
					c.Request.Header.Set("Authorization", "Bearer project-user-key")
					c.Request.Header.Set("X-Codex-Project-Id", original)
					c.Request.Header.Set("X-Codex-Workspace-Id", original)
					handler.Responses(c)
					output = recorder.Body.String()
				}
				var wire []byte
				var wireHeaders http.Header
				select {
				case packet := <-captured:
					wire, wireHeaders = packet.body, packet.headers
				case <-time.After(time.Second):
					t.Fatalf("no upstream request: %s", output)
				}
				require.NotContains(t, string(wire), original)
				require.NotContains(t, string(wire), "project_mapping")
				require.Contains(t, output, original)
				alias := ""
				for _, item := range gjson.GetBytes(wire, "input").Array() {
					if item.Get("role").String() == "user" {
						alias = item.Get("content.0.text").String()
					}
				}
				require.NotEmpty(t, alias)
				require.NotEqual(t, original, alias)
				require.NotContains(t, output, alias)
				require.NoError(t, proxy.ValidateCodexOutboundMetadata(wire, wireHeaders))
				for _, values := range wireHeaders {
					for _, value := range values {
						require.NotContains(t, value, original)
						require.NotContains(t, value, "project_mapping")
					}
				}
				if turn == 0 {
					for _, field := range []string{"project_id", "workspace_id"} {
						require.Equal(t, alias, gjson.GetBytes(wire, "client_metadata."+field).String())
						require.Equal(t, alias, gjson.Get(gjson.GetBytes(wire, "client_metadata.x-codex-turn-metadata").String(), field).String())
						if metadata := wireHeaders.Get("X-Codex-Turn-Metadata"); metadata != "" {
							require.Equal(t, alias, gjson.Get(metadata, field).String())
						}
					}
				} else {
					require.False(t, gjson.GetBytes(wire, "client_metadata.project_id").Exists())
					require.Empty(t, wireHeaders.Get("X-Codex-Project-Id"))
				}
				if turn > 0 {
					require.Equal(t, previousAlias, alias)
				}
				previousAlias = alias
				body, _ = sjson.DeleteBytes(body, "client_metadata.project_id")
				body, _ = sjson.DeleteBytes(body, "client_metadata.workspace_id")
				body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-metadata.project_id")
				body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-metadata.workspace_id")
			}
		})
	}
}
