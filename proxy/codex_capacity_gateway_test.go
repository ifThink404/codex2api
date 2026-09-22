package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestCodexCapacityGatewayAttemptsAndFinalError(test *testing.T) {
	for _, transient := range []bool{false, true} {
		test.Run(fmt.Sprintf("transient=%t", transient), func(test *testing.T) {
			testCodexCapacityGatewayAttemptsAndFinalError(test, transient)
		})
	}
}

func testCodexCapacityGatewayAttemptsAndFinalError(test *testing.T, transient bool) {
	gin.SetMode(gin.TestMode)
	fixture := codexCapacityTestBody
	if transient {
		fixture = codexTransientOverloadTestBody
	}
	for _, ingress := range []string{"http", "http-nonstream", "ws"} {
		for _, upstream := range []string{"http-error", "stream-error", "handshake-error", "partial-stream"} {
			for _, enabled := range []bool{false, true} {
				for _, continuous := range []bool{false, true} {
					test.Run(fmt.Sprintf("%s/%s/enabled=%t/continuous=%t", ingress, upstream, enabled, continuous), func(test *testing.T) {
						previous := CurrentRuntimeSettings()
						previousExecutor := WebsocketExecuteFunc
						test.Cleanup(func() {
							ApplyRuntimeSettings(previous)
							WebsocketExecuteFunc = previousExecutor
						})
						next := previous
						next.CodexForceWebsocket = true
						next.CodexCapacityRetryEnabled = enabled
						next.CodexWSSilentRetry = true
						next.CodexWSSilentRetries = 1
						next.CodexWSHideErrors = true
						next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: continuous, CatchAll: continuous, MaxDurationSeconds: 1}
						ApplyRuntimeSettings(next)
						var attempts atomic.Int64
						WebsocketExecuteFunc = func(_ context.Context, _ *auth.Account, _ []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
							attempts.Add(1)
							if upstream == "handshake-error" {
								return nil, continuousRetryTestHTTPError{status: 500, body: []byte(fixture)}
							}
							status, body := 500, fixture
							if upstream != "http-error" {
								status = 200
								body = "data: " + `{"type":"response.created","response":{"id":"resp_capacity"}}` + "\n\n"
								if upstream == "partial-stream" {
									body += "data: " + `{"type":"response.output_text.delta","delta":"partial"}` + "\n\n"
								}
								body += "data: " + `{"type":"error",` + fixture[1:] + "\n\n"
								body += "data: " + `{"type":"response.failed","response":` + fixture + "}\n\n"
							}
							return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
						}
						store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 1, MaxRateLimitRetries: 1, TransportRetryPolicy: "sticky"})
						test.Cleanup(store.Stop)
						store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", AccountID: "test-account", PlanType: "pro"})
						handler := NewHandler(store, nil, nil, nil)
						body := `{"type":"response.create","model":"gpt-5.5","input":"hello","stream":` + fmt.Sprint(ingress != "http-nonstream") + `}`
						visiblePartial := upstream == "partial-stream" && !continuous && ingress != "http-nonstream"
						if ingress == "ws" {
							router := gin.New()
							router.GET("/v1/responses", handler.ResponsesWebSocket)
							server := httptest.NewServer(router)
							test.Cleanup(server.Close)
							connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
							if err != nil {
								test.Fatal(err)
							}
							test.Cleanup(func() { _ = connection.Close() })
							_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
							if err := connection.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
								test.Fatal(err)
							}
							for {
								_, payload, err := connection.ReadMessage()
								if err != nil {
									test.Fatalf("missing capacity terminal: %v", err)
								}
								if event := gjson.GetBytes(payload, "type").String(); event == "error" || event == "response.failed" {
									path := "error"
									if event == "response.failed" {
										path = "response.error"
									}
									if transient {
										if event != "response.failed" {
											test.Fatalf("retry advice must use response.failed: %s", payload)
										}
										assertCodexTransientOverload(test, payload, path)
									} else {
										assertCodexCapacityError(test, payload, path)
									}
									break
								}
							}
							if !visiblePartial {
								_, _, err := connection.ReadMessage()
								wantClose := websocket.ClosePolicyViolation
								if transient {
									wantClose = websocket.CloseTryAgainLater
								}
								if !websocket.IsCloseError(err, wantClose) {
									test.Fatalf("capacity close=%v, want %d", err, wantClose)
								}
							}
						} else {
							recorder := httptest.NewRecorder()
							ctx, _ := gin.CreateTestContext(recorder)
							requestCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(requestCtx)
							ctx.Request.Header.Set("Content-Type", "application/json")
							handler.Responses(ctx)
							if transient {
								if ingress == "http-nonstream" {
									if recorder.Code != 500 || recorder.Header().Get("Retry-After") != "30" {
										test.Fatalf("transient JSON: status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body)
									}
									assertCodexTransientOverload(test, recorder.Body.Bytes(), "error")
								} else {
									if recorder.Code != 200 || !strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
										test.Fatalf("retry advice must be an SSE failure: %d %s", recorder.Code, recorder.Body)
									}
									found := false
									for _, line := range strings.Split(recorder.Body.String(), "\n") {
										payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
										if gjson.Get(payload, "type").String() == "response.failed" {
											assertCodexTransientOverload(test, []byte(payload), "response.error")
											found = true
										}
									}
									if !found || strings.Contains(recorder.Body.String(), "server_is_overloaded") {
										test.Fatalf("missing retryable terminal: %s", recorder.Body)
									}
								}
							} else if visiblePartial {
								if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"code":"server_is_overloaded"`) || strings.Contains(recorder.Body.String(), `"code":"server_error"`) {
									test.Fatalf("stream error rewritten: %d %s", recorder.Code, recorder.Body.String())
								}
							} else {
								if recorder.Code != 400 {
									test.Fatalf("HTTP status=%d body=%s", recorder.Code, recorder.Body.String())
								}
								assertCodexCapacityError(test, recorder.Body.Bytes(), "error")
							}
						}
						wantAttempts := int64(1)
						if enabled && !visiblePartial {
							wantAttempts = 2
						}
						if attempts.Load() != wantAttempts {
							test.Fatalf("upstream attempts=%d, want %d", attempts.Load(), wantAttempts)
						}
					})
				}
			}
		}
	}
}
