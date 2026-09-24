package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestInitialSessionAdmissionRemovedForSever(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, disabled := range []bool{false, true} {
		settings := previous
		settings.CodexInitialSessionAgeCheckDisabled = disabled
		settings.CodexInitialSessionMaxAgeSeconds = 1
		ApplyRuntimeSettings(settings)
		for _, sample := range []struct {
			name, id string
			native   bool
		}{
			{"reported_uuid_v4", "75767386-0587-4ccb-a6c6-2c4fd366eb45", false},
			{"opaque_session", "ordinary-sdk-session", false},
			{"native_uuid_v4", "75767386-0587-4ccb-a6c6-2c4fd366eb45", true},
			{"expired_uuid_v7", initialTestID(time.Now().Add(-365 * 24 * time.Hour)), true},
			{"future_uuid_v7", initialTestID(time.Now().Add(365 * 24 * time.Hour)), true},
		} {
			t.Run(sample.name+"/age_check_disabled="+strconv.FormatBool(disabled), func(t *testing.T) {
				h := newWindowAuthorizationHandler(t)
				config := h.store.GetPromptFilterConfig()
				config.Advanced.Risk.SessionContinuityMode = "observe"
				h.store.SetPromptFilterConfig(config)
				newRequest := func() (*gin.Context, []byte, requestSessionIdentity) {
					request, body := continuityTestRequest(0, "turn")
					if sample.native {
						body = []byte(strings.ReplaceAll(string(body), continuityTestThread, sample.id))
					} else {
						body = []byte(`{"model":"gpt-5.6-sol","input":"1"}`)
					}
					request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
					beginDispatchSelection(request)
					request.Request.Header.Set("Session_id", sample.id)
					request.Request.Header.Set("User-Agent", "Go-http-client/1.1")
					usageRequestDiagnosticState(request).StartedAt = time.Now()
					return request, body, h.resolveRequestSessionIdentityForContext(request, body)
				}
				request, body, identity := newRequest()
				require.True(t, identity.stableIdentity)
				key := capacityAwareSessionAffinityKey(identity, 101)
				require.NotEmpty(t, key)
				require.Nil(t, h.prepareSessionContinuity(request, identity, key, body))
				require.NotNil(t, continuityRequest(request), "ordinary API sessions still retain account affinity")
				account := &auth.Account{DBID: 99, AccessToken: "dummy", Status: auth.StatusReady}
				h.store.AddAccount(account)
				require.Nil(t, h.commitSessionContinuity(request, account))
				next, nextBody, nextIdentity := newRequest()
				require.Nil(t, h.prepareSessionContinuity(next, nextIdentity, key, nextBody))
				require.NotNil(t, continuityRequest(next), "next identity: %+v; resolution: %+v", nextIdentity, usageRequestDiagnosticState(next).Resolved)
				require.Equal(t, account.ID(), selectionTraceForRequest(next).PinnedAccount())
			})
		}
	}
}
