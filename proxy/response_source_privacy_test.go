package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/internal/upstreamprivacy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestNewAPISourceVisibilityRequiresVerifiedMetadata(t *testing.T) {
	for _, scenario := range []string{"admin", "ordinary", "tampered_meta", "unsigned", "wrong_key", "wrong_platform"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := promptGuardTestConfig()
			cfg.Advanced.NewAPI.Enabled = true
			h := newPromptGuardTestHandler(cfg)
			body := []byte(`{"input":"hello","preserve_upstream_source":true}`)
			c, _ := signedNewAPIPolicyContext(t, "source-visibility-"+scenario, newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
			meta := newAPIPolicyMeta{Profile: "balanced", Mode: "enforce", Provider: "codex2api", Protocol: "responses", PreserveUpstreamSource: scenario != "ordinary"}
			if scenario == "wrong_platform" {
				meta.PlatformID = "another-platform"
			}
			addSignedNewAPIPolicyMeta(t, c, meta, true)
			c.Request.Header.Set("X-NewAPI-Preserve-Upstream-Source", "true")
			c.Request.Header.Set("X-NewAPI-User-Role", "100")
			switch scenario {
			case "tampered_meta":
				meta.PreserveUpstreamSource = false
				addSignedNewAPIPolicyMeta(t, c, meta, true)
				payload, err := base64.RawURLEncoding.DecodeString(c.GetHeader("X-NewAPI-Policy-Meta"))
				require.NoError(t, err)
				payload, err = sjson.SetBytes(payload, "preserve_upstream_source", true)
				require.NoError(t, err)
				c.Request.Header.Set("X-NewAPI-Policy-Meta", base64.RawURLEncoding.EncodeToString(payload))
			case "unsigned":
				c.Request.Header.Del("X-NewAPI-Signature")
			case "wrong_key":
				c.Set(contextAPIKeyID, int64(999))
			}
			h.primeNewAPIPolicyContext(c, body)
			h.resolveRequestSessionIdentityForContext(c, body)
			assert.Equal(t, scenario == "admin", preserveUpstreamSource(c.Request.Context()))
			if scenario == "admin" {
				// Reclassification for another WS turn must actively clear it.
				bindUpstreamSourceVisibility(c, verifiedNewAPIPolicyContext{}, false)
				assert.False(t, preserveUpstreamSource(c.Request.Context()))
			}
		})
	}
}

func TestNewAPIAdminResponseSourceSurvivesAllBodyBoundaries(t *testing.T) {
	const address = "https://" + "bp" + "s.openai.com/" + "basis" + "points/api/responses"
	const text = "Basis Points: " + address
	for _, admin := range []bool{false, true} {
		for _, bps := range []bool{false, true} {
			for _, transport := range []string{"json", "compact", "sse"} {
				t.Run(fmt.Sprintf("admin=%t/bps=%t/%s", admin, bps, transport), func(t *testing.T) {
					cfg := promptGuardTestConfig()
					cfg.Advanced.NewAPI.Enabled = true
					h := newPromptGuardTestHandler(cfg)
					requestBody := []byte(`{"input":"hello"}`)
					c, _ := signedNewAPIPolicyContext(t, "source-boundary", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", requestBody)
					addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{Profile: "balanced", Mode: "enforce", Provider: "codex2api", Protocol: "responses", PreserveUpstreamSource: admin}, true)
					h.primeNewAPIPolicyContext(c, requestBody)
					h.resolveRequestSessionIdentityForContext(c, requestBody)
					if bps {
						c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), codexBPSDiagnosticKey{}, &CodexBPSDiagnostic{Mode: "bps", projection: newBPSResponseProjection(nil)}))
					}
					arguments, err := json.Marshal(map[string]string{"url": address})
					require.NoError(t, err)
					object := "response"
					if transport == "compact" {
						object = "response.compaction"
					}
					response := map[string]any{
						"object": object, "status": "completed", "model": "gpt-6-astra",
						"account_id": "private-account", "access_token": "private-token", "session_id": "private-session",
						"usage": map[string]int{"input_tokens": 10, "output_tokens": 20},
						"output": []any{
							map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": text}}},
							map[string]any{"type": "function_call", "name": "echo", "call_id": "call_1", "arguments": string(arguments)},
						},
					}
					encoded, err := json.Marshal(response)
					require.NoError(t, err)
					contentType := "application/json"
					body := string(encoded)
					if transport == "sse" {
						contentType = "text/event-stream"
						var frames bytes.Buffer
						for _, delta := range []struct{ kind, value string }{
							{"response.output_text.delta", text},
							{"response.function_call_arguments.delta", string(arguments)},
						} {
							// Split inside the hostname; both buffering and final writes matter.
							split := strings.Index(delta.value, "bp"+"s.openai.com") + 2
							for _, part := range []string{delta.value[:split], delta.value[split:]} {
								frame, err := json.Marshal(map[string]string{"type": delta.kind, "item_id": delta.kind, "delta": part})
								require.NoError(t, err)
								fmt.Fprintf(&frames, "data: %s\n\n", frame)
							}
						}
						fmt.Fprintf(&frames, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", encoded)
						body = frames.String()
					}
					upstream := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
					require.NoError(t, maskTurnStateResponse(c.Request.Context(), nil, upstream))
					output, err := io.ReadAll(upstream.Body)
					require.NoError(t, err)
					require.NoError(t, upstream.Body.Close())
					wantText, wantAddress := text, address
					if !admin {
						wantText, wantAddress = upstreamprivacy.Text(text), upstreamprivacy.Text(address)
						if bps {
							wantText = upstreamprivacy.SourceText(text)
						}
					}
					payloads := []string{string(output)}
					if transport == "sse" {
						payloads = nil
						for _, line := range strings.Split(string(output), "\n") {
							if strings.HasPrefix(line, "data: ") {
								payloads = append(payloads, strings.TrimPrefix(line, "data: "))
							}
						}
					}
					var textDeltas, argumentDeltas string
					for _, payload := range payloads {
						// This is also the final Responses WebSocket event boundary.
						public := publicResponseErrorPayload(c, []byte(payload))
						assert.NotContains(t, string(public), "private-account")
						assert.NotContains(t, string(public), "private-token")
						assert.NotContains(t, string(public), "private-session")
						node := gjson.ParseBytes(public)
						switch node.Get("type").String() {
						case "response.output_text.delta":
							textDeltas += node.Get("delta").String()
							continue
						case "response.function_call_arguments.delta":
							argumentDeltas += node.Get("delta").String()
							continue
						case "response.completed":
							node = node.Get("response")
						}
						assert.Equal(t, wantText, node.Get("output.0.content.0.text").String())
						assert.Equal(t, wantAddress, gjson.Get(node.Get("output.1.arguments").String(), "url").String())
						assert.Equal(t, "gpt-6-astra", node.Get("model").String())
						assert.EqualValues(t, 20, node.Get("usage.output_tokens").Int())
					}
					if transport == "sse" {
						assert.Equal(t, wantText, textDeltas)
						assert.Equal(t, wantAddress, gjson.Get(argumentDeltas, "url").String())
					}
				})
			}
		}
	}
}

func TestNewAPIAdminSourceVisibilityThroughWebSocket(t *testing.T) {
	const address = "https://" + "bp" + "s.openai.com/" + "basis" + "points/api/responses"
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			h, _, _, _ := failoverTestSetup(t, true)
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			oldResin := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(oldResin) })
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":%q}\n\n", address)
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_source_ws\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", address)
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "source-privacy-local"})
			engine := gin.New()
			engine.GET("/v1/responses", func(c *gin.Context) {
				c.Set(contextAPIKeyID, int64(101))
				h.ResponsesWebSocket(c)
			})
			server := httptest.NewServer(engine)
			t.Cleanup(server.Close)
			request, _ := gin.CreateTestContext(httptest.NewRecorder())
			request.Request = httptest.NewRequest(http.MethodGet, server.URL+"/v1/responses", nil)
			setSignedNewAPIRequestHeaders(t, request.Request, nil, "source-websocket", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "test-platform", "integration-secret", "")
			addSignedNewAPIPolicyMeta(t, request, newAPIPolicyMeta{
				Profile: "balanced", Mode: "enforce", Provider: "codex2api", Protocol: "responses", PreserveUpstreamSource: admin,
				RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable,
			}, true)
			request.Request.Header.Set("Authorization", "Bearer test-user-key")
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", request.Request.Header)
			require.NoError(t, err)
			defer conn.Close()
			_, body := failoverTestRequest(t, h)
			body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(uuid.Must(uuid.NewV7()).String()))
			body, err = sjson.SetBytes(body, "type", "response.create")
			require.NoError(t, err)
			for turn := 0; turn < 2; turn++ {
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				want := address
				if !admin {
					want = upstreamprivacy.Text(address)
				}
				var deltas string
				for {
					_, frame, err := conn.ReadMessage()
					require.NoError(t, err)
					kind := gjson.GetBytes(frame, "type").String()
					require.NotEqual(t, "error", kind, "%s", frame)
					require.NotEqual(t, "response.failed", kind, "%s", frame)
					if kind == "response.output_text.delta" {
						deltas += gjson.GetBytes(frame, "delta").String()
					}
					if kind == "response.completed" {
						assert.Equal(t, want, gjson.GetBytes(frame, "response.output.0.content.0.text").String())
						break
					}
				}
				assert.Equal(t, want, deltas)
			}
		})
	}
}
