package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionModelGuidanceOnlyRecommendsEligibleBoundModel(t *testing.T) {
	for _, scenario := range []string{"supported", "key_denied", "key_allow_other", "no_key_metadata", "owner_unsupported", "owner_cooldown", "model_cooldown", "other_scope", "mapped_to_unavailable", "mapped_to_allowed", "channel_mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRootlessPassiveModelTestHandler(t)
			account := &auth.Account{DBID: 77, AccessToken: "private-token", AccountID: "private-account", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			h.store.AddAccount(account)
			row := &database.APIKeyRow{ID: 101}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Set(contextAPIKeyID, int64(101))
			c.Set(contextAPIKeyRow, row)
			beginDispatchSelection(c)
			const key = "existing-session"
			h.store.BindSessionAffinity(key, account, "")
			switch scenario {
			case "key_denied":
				row.Limits.ModelDeny = []string{"gpt-5.6-sol"}
			case "key_allow_other":
				row.Limits.ModelAllow = []string{"gpt-6-astra"}
			case "no_key_metadata":
				c.Set(contextAPIKeyRow, nil)
			case "owner_unsupported":
				account.Models = []string{"gpt-5.6-terra"}
				// A different pool account cannot justify an in-conversation hint.
				h.store.AddAccount(&auth.Account{DBID: 78, AccessToken: "other", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}})
			case "owner_cooldown":
				account.Status, account.CooldownUtil = auth.StatusCooldown, time.Now().Add(time.Hour)
			case "model_cooldown":
				account.SetModelCooldownUntil("gpt-5.6-sol", "rate_limit", time.Now().Add(time.Hour))
			case "other_scope":
				h.store.SetAPIKeyAllowedGroups(row.ID, []int64{99})
			case "mapped_to_unavailable":
				h.store.SetCodexModelMapping(`{"gpt-5.6-sol":"gpt-6-astra"}`)
			case "mapped_to_allowed":
				h.store.SetCodexModelMapping(`{"gpt-5.6-sol":"gpt-5.6-terra"}`)
				account.Models = []string{"gpt-5.6-terra"}
			case "channel_mismatch":
				row.Limits.UpstreamChannel = database.UpstreamChannelClaude
			}
			err := h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-6-astra", "gpt-6-astra", false)
			require.NotNil(t, err)
			require.Equal(t, api.ErrCodeSessionModelUnavailable, err.Code)
			require.Contains(t, err.Message, "当前对话无法继续使用 gpt-6-astra")
			require.Contains(t, err.Message, "请新建对话后重试")
			if scenario == "supported" || scenario == "mapped_to_allowed" {
				require.Contains(t, err.Message, "可尝试切换至 gpt-5.6-sol")
			} else {
				require.NotContains(t, err.Message, "gpt-5.6-sol")
			}
			for _, private := range []string{"private-token", "private-account", "existing-session", "上游账号", "降智", "质量异常"} {
				require.NotContains(t, err.Message, private)
			}
			require.Nil(t, err.Details)
			require.Equal(t, "false", recorder.Header().Get("X-Should-Retry"))
			owner, found := h.store.SessionAffinityAccountID(key)
			require.True(t, found)
			require.Equal(t, account.ID(), owner)
		})
	}
}

func TestSessionModelGuidanceSurvivesCommittedStreams(t *testing.T) {
	for _, protocol := range []continuousRetryHTTPProtocol{continuousRetryProtocolResponses, continuousRetryProtocolChat, continuousRetryProtocolAnthropic} {
		t.Run(string(rune('0'+protocol)), func(t *testing.T) {
			h := newRootlessPassiveModelTestHandler(t)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			beginDispatchSelection(c)
			trace := selectionTraceForRequest(c)
			trace.SetSessionModelFilter(func(*auth.Account) bool { return false })
			require.False(t, trace.CheckSessionModel(&auth.Account{DBID: 7}))
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString(": heartbeat\n\n")
			c.Writer.Flush()
			if protocol == continuousRetryProtocolAnthropic {
				sendSessionModelError(c, sessionModelErrorForRequest(c), protocol)
			} else {
				h.sendDispatchUnavailable(c, true, protocol == continuousRetryProtocolChat)
			}
			output := recorder.Body.String()
			require.Equal(t, http.StatusOK, recorder.Code)
			require.NotContains(t, output, publicUpstreamFailureMessage)
			data := strings.SplitN(output, "data: ", 2)
			require.Len(t, data, 2)
			path := "error"
			if protocol == continuousRetryProtocolResponses {
				path = "response.error"
				require.Equal(t, "response.failed", gjson.Get(data[1], "type").String())
			}
			require.Equal(t, "session_model_unavailable", gjson.Get(data[1], path+".code").String())
			require.Equal(t, sessionModelUnavailableMessage, gjson.Get(data[1], path+".message").String())
			if protocol == continuousRetryProtocolAnthropic {
				require.Contains(t, output, "event: error\n")
			}
		})
	}
}

func TestSessionFailoverCapacityGuidanceIsDistinctFromModelUnavailable(t *testing.T) {
	for _, reason := range []string{"account_session_capacity_full", "account_usage_exhausted"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		plan := &sessionAccountFailoverPlan{Diagnostic: &sessionAccountFailoverDiagnostic{Result: "no_safe_candidate", TriggerReason: reason}}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), sessionAccountFailoverContextKey{}, plan))
		failure := sessionFailoverUnavailableAPIError(c)
		require.Equal(t, api.ErrCodeServiceUnavailable, failure.Code)
		if reason == "account_session_capacity_full" {
			require.Equal(t, sessionFailoverCapacityMessage, failure.Message)
		} else {
			require.Equal(t, sessionFailoverUnavailableMessage, failure.Message)
		}
		require.NotContains(t, failure.Message, "gpt-5.6")
	}
}

func TestSessionModelGuidanceWebsocketClosePreservesUTF8(t *testing.T) {
	message := "当前对话无法继续使用 gpt-6-astra。可尝试切换至 gpt-5.6-sol 继续当前任务；如需使用 gpt-6-astra，请新建对话后重试。"
	closeReason := truncateWebSocketCloseReason(message)
	require.True(t, utf8.ValidString(closeReason))
	require.LessOrEqual(t, len(closeReason), 120)
	require.True(t, strings.HasPrefix(message, closeReason))
}

func TestSessionModelGuidanceAfterFailoverAndWhitelistChange(t *testing.T) {
	for _, switched := range []bool{false, true} {
		t.Run(map[bool]string{false: "whitelist_removed", true: "restored_owner"}[switched], func(t *testing.T) {
			// Guidance remains for administrators who intentionally disable failover.
			h, old, current, key := failoverTestSetup(t, false)
			old.Models = []string{"gpt-6-astra", "gpt-5.6-sol"}
			if switched {
				_, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: old.ID(), AccountID: current.ID(), Reason: "account_session_capacity_full"})
				require.NoError(t, err)
			} else {
				old.Models = []string{"gpt-5.6-sol"}
				current = old
			}
			c, body := failoverTestRequest(t, h)
			body, err := sjson.SetBytes(body, "model", "gpt-6-astra")
			require.NoError(t, err)
			c.Set(ingressRequestBodyContextKey, body)
			c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 101})
			failure := h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-6-astra", "gpt-6-astra", false, body)
			require.NotNil(t, failure)
			require.Equal(t, api.ErrCodeSessionModelUnavailable, failure.Code)
			require.Contains(t, failure.Message, "可尝试切换至 gpt-5.6-sol")
			require.Equal(t, current.ID(), selectionTraceForRequest(c).PinnedAccount())
			record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, current.ID(), record.AccountID)
			// Choosing the suggested model follows the existing bound owner.
			next, nextBody := failoverTestRequest(t, h)
			require.Nil(t, h.configureSessionModelAffinity(next, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, nextBody))
			require.Equal(t, current.ID(), selectionTraceForRequest(next).PinnedAccount())
		})
	}
}
