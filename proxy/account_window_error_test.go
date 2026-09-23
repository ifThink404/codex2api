package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountWindowErrorHTTPAndWebSocket(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, scenario := range []string{"quota", "unknown_reset", "healthy_capacity_full"} {
			t.Run(transport+"/"+scenario, func(t *testing.T) {
				store := auth.NewStore(nil, nil, nil)
				t.Cleanup(store.Stop)
				recovery := time.Now().Add(90 * time.Second).Truncate(time.Second)
				limited := &auth.Account{DBID: 1, AccessToken: "test", PlanType: "pro", UsagePercent5hValid: true, UsagePercent5h: 100, Reset5hAt: recovery}
				if scenario == "unknown_reset" {
					limited.PlanType, limited.UsagePercent7dValid, limited.UsagePercent7d = "free", true, 100
				}
				store.AddAccount(limited)
				wantCode, wantStatus := api.ErrCodeRateLimitReached, http.StatusTooManyRequests
				if scenario == "healthy_capacity_full" {
					full := &auth.Account{DBID: 2, AccessToken: "test-2", PlanType: "pro", SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600}
					store.AddAccount(full)
					require.True(t, store.AdmitAccountSession(full, "occupied", time.Now()))
					wantCode, wantStatus = api.ErrCodeAccountSessionCapacity, http.StatusBadRequest
				}
				handler := NewHandler(store, nil, nil, nil)
				sessionID := initialTestID(time.Now())
				var payload []byte
				if transport == "http" {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello"}`))
					c.Request.Header.Set("Session-Id", sessionID)
					handler.Responses(c)
					require.Equal(t, wantStatus, recorder.Code, recorder.Body.String())
					if scenario == "quota" {
						seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
						require.NoError(t, err)
						require.Positive(t, seconds)
						require.LessOrEqual(t, seconds, 90)
					} else {
						require.Empty(t, recorder.Header().Get("Retry-After"))
					}
					payload = recorder.Body.Bytes()
				} else {
					router := gin.New()
					router.GET("/v1/responses", handler.ResponsesWebSocket)
					server := httptest.NewServer(router)
					defer server.Close()
					conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Session-Id": []string{sessionID}})
					require.NoError(t, err)
					defer conn.Close()
					require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hello"}`)))
					require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
					_, payload, err = conn.ReadMessage()
					require.NoError(t, err)
				}
				require.Equal(t, string(wantCode), gjson.GetBytes(payload, "error.code").String(), string(payload))
				message := gjson.GetBytes(payload, "error.message").String()
				switch scenario {
				case "quota":
					require.Contains(t, message, recovery.UTC().Format("2006-01-02 15:04:05 UTC"))
					require.Contains(t, message, "秒后")
				case "unknown_reset":
					require.Contains(t, message, "暂未获取到恢复时间")
					require.NotContains(t, message, "UTC")
				case "healthy_capacity_full":
					require.Equal(t, accountSessionCapacityExceededMessage, message)
				}
			})
		}
	}
}

func TestAccountWindowErrorCommittedSSEPreservesRecovery(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	recovery := time.Now().Add(90 * time.Second).Truncate(time.Second)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test", PlanType: "pro", UsagePercent5hValid: true, UsagePercent5h: 100, Reset5hAt: recovery})
	handler := NewHandler(store, nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	_, err := c.Writer.WriteString(continuousRetryKeepaliveComment)
	require.NoError(t, err)
	c.Writer.Flush()
	failure := handler.accountWindowUnavailableAPIError(c, 0, nil, nil, auth.DispatchPolicyStandard, "new")
	require.NotNil(t, failure)
	require.True(t, writeCommittedResponsesLocalError(c, failure))
	payload := strings.TrimSpace(strings.SplitN(recorder.Body.String(), "data: ", 2)[1])
	require.Equal(t, "response.failed", gjson.Get(payload, "type").String())
	require.Equal(t, string(api.ErrCodeRateLimitReached), gjson.Get(payload, "response.error.code").String())
	require.Equal(t, failure.Message, gjson.Get(payload, "response.error.message").String())
	require.Contains(t, payload, recovery.UTC().Format("2006-01-02 15:04:05 UTC"))
	require.Empty(t, recorder.Result().Header.Get("Retry-After"), "committed streams carry recovery in the event, not late headers")
}
