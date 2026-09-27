package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestDispatchRecoveryOwnerFailureHTTP(t *testing.T) {
	for _, scenario := range []string{"model", "unauthorized", "credential", "cooldown"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, target, _ := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			var oldCalls, newCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Header.Get("Chatgpt-Account-Id") == owner.AccountID {
					oldCalls.Add(1)
				} else {
					newCalls.Add(1)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				stickyFailureSuccess(w)
			}))
			defer server.Close()
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "owner-failure-audit"})
			atomic.StoreInt32(&target.Disabled, 1)
			thread, err := uuid.NewV7()
			require.NoError(t, err)
			_, body := failoverTestRequest(t, h)
			body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(thread.String()))
			body, err = sjson.SetBytes(body, "stream", true)
			require.NoError(t, err)
			request := func() (*gin.Context, *httptest.ResponseRecorder) {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Authorization", "Bearer test-user-key")
				c.Request.Header.Set("Content-Type", "application/json")
				ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				h.Responses(c)
				return c, w
			}
			_, first := request()
			require.Equal(t, 200, first.Code, first.Body.String())
			require.EqualValues(t, 1, oldCalls.Load())
			atomic.StoreInt32(&target.Disabled, 0)
			switch scenario {
			case "model":
				owner.Models = []string{"gpt-5.6-terra"}
			case "credential":
				owner.AccessToken = ""
			case "unauthorized", "cooldown":
				owner.Status, owner.CooldownReason, owner.CooldownUtil = auth.StatusCooldown, map[string]string{"unauthorized": "unauthorized", "cooldown": "server_error"}[scenario], time.Now().Add(time.Hour)
			}
			c, w := request()
			fields := map[string]any{"scenario": "http_" + scenario, "status": w.Code, "old_calls": oldCalls.Load(), "target_calls": newCalls.Load()}
			if d := usageRequestDiagnosticState(c).AccountFailover; d != nil {
				fields["result"] = d.Result
				fields["reason"] = d.Reason
			}
			dispatchRecoveryLog(t, fields)
			require.Equal(t, 200, w.Code, "existing session must recover using the healthy replacement")
			require.EqualValues(t, 1, newCalls.Load())
		})
	}
}
