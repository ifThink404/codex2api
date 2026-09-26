package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestSessionFailoverAcrossIngressProtocols(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages", "websocket"} {
		for _, trigger := range []string{"model", "retry"} {
			t.Run(path+"/"+trigger, func(t *testing.T) {
				h, owner, target, _ := failoverTestSetup(t, true)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
					s.CodexWSSilentRetry, s.CodexWSSilentRetries = true, 1
					return s
				})
				h.store.SetMaxRetries(1)
				h.store.SetMaxRateLimitRetries(1)
				h.store.SetRetryIntervalMS(1)
				h.store.SetTransportRetryPolicy("rotate")
				t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
				oldResin := GetResinConfig()
				t.Cleanup(func() { SetResinConfig(oldResin) })
				var failing atomic.Bool
				seen := make(chan string, 8)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					token := r.Header.Get("Authorization")
					seen <- token
					if failing.Load() && token == "Bearer owner-token" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(429)
						io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"temporary throttle"}}`)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/compact") {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"id":"compact-result","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"new-context"}]}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					stickyFailureSuccess(w)
				}))
				defer upstream.Close()
				SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "failover-protocol"})
				_, body := failoverTestRequest(t, h)
				body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(NewUpstreamSessionUUID()))
				body, _ = sjson.SetBytes(body, "stream", true)
				send := func(endpoint string, raw []byte) (*gin.Context, *httptest.ResponseRecorder) {
					out := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(out)
					c.Request = httptest.NewRequest("POST", endpoint, bytes.NewReader(raw))
					c.Request.Header.Set("Authorization", "Bearer test-user-key")
					c.Request.Header.Set("Content-Type", "application/json")
					ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
					defer cancel()
					c.Request = c.Request.WithContext(ctx)
					map[string]func(*gin.Context){"/v1/responses": h.Responses, "/v1/responses/compact": h.ResponsesCompact, "/v1/chat/completions": h.ChatCompletions, "/v1/messages": h.Messages}[endpoint](c)
					return c, out
				}
				atomic.StoreInt32(&target.Disabled, 1)
				_, initial := send("/v1/responses", body)
				require.Equal(t, 200, initial.Code, initial.Body.String())
				require.Contains(t, initial.Body.String(), "response.completed")
				require.Equal(t, "Bearer owner-token", <-seen)
				atomic.StoreInt32(&target.Disabled, 0)
				if trigger == "model" {
					owner.Models = []string{"gpt-5.6-terra"}
				} else {
					failing.Store(true)
				}
				if path == "/v1/chat/completions" || path == "/v1/messages" {
					body, _ = sjson.DeleteBytes(body, "input")
					body, _ = sjson.SetRawBytes(body, "messages", []byte(`[{"role":"user","content":"Full plaintext conversation"}]`))
					body, _ = sjson.SetBytes(body, "max_tokens", 32)
				}
				if path == "/v1/responses/compact" {
					body, _ = sjson.SetBytes(body, "stream", false)
				}
				if path == "websocket" {
					body, _ = sjson.SetBytes(body, "type", "response.create")
					router := gin.New()
					router.GET("/v1/responses", h.ResponsesWebSocket)
					server := httptest.NewServer(router)
					defer server.Close()
					conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-user-key"}})
					require.NoError(t, err)
					defer conn.Close()
					require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
					require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
					for {
						_, event, err := conn.ReadMessage()
						require.NoError(t, err, string(event))
						require.NotContains(t, string(event), `"type":"error"`)
						if bytes.Contains(event, []byte("response.completed")) {
							break
						}
					}
				} else {
					_, out := send(path, body)
					require.Equal(t, 200, out.Code, out.Body.String())
					require.NotContains(t, out.Body.String(), `"error":`)
				}
				expectedAttempts := 1
				if trigger == "retry" {
					expectedAttempts = 2
				}
				require.Len(t, seen, expectedAttempts)
				if trigger == "retry" {
					require.Equal(t, "Bearer owner-token", <-seen)
				}
				require.Equal(t, "Bearer target-token", <-seen)
				require.Empty(t, seen)
			})
		}
	}
}
