package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Exercise the public handlers: the owner is healthy at selection time and
// only becomes exhausted after the upstream request has actually been sent.
func TestSessionQuotaRetryEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name                                                         string
		native, streamFailure, temporary, preserve, visible, compact bool
		headerQuota, targetQuota, temporaryAlways                    bool
		budget                                                       int
		relaxed                                                      bool
		disabled                                                     bool
		targetMismatch                                               string
	}{
		{name: "http_429", budget: 1},
		{name: "header_only_quota", budget: 1, headerQuota: true},
		{name: "native_header_only_quota", native: true, budget: 1, headerQuota: true},
		{name: "retry_target_also_exhausted", budget: 1, targetQuota: true},
		{name: "http_sse", streamFailure: true, budget: 1},
		{name: "native_http_429", native: true, budget: 1},
		{name: "native_stream_error", native: true, streamFailure: true, budget: 1},
		{name: "preserve_input", streamFailure: true, preserve: true, budget: 1},
		{name: "compact_429", compact: true, budget: 1},
		{name: "compact_sse_quota", compact: true, streamFailure: true, budget: 1},
		{name: "compact_sse_temporary", compact: true, streamFailure: true, temporary: true, budget: 1},
		{name: "zero_budget", budget: 0},
		{name: "native_zero_budget", native: true, streamFailure: true, budget: 0},
		{name: "failover_disabled", budget: 1, disabled: true},
		{name: "temporary_429_sticky", budget: 1, temporary: true},
		{name: "relaxed_temporary_429_rotate", budget: 1, temporary: true, relaxed: true},
		{name: "relaxed_temporary_stream_rotate", budget: 1, temporary: true, streamFailure: true, relaxed: true},
		{name: "relaxed_compact_temporary_rotate", budget: 1, compact: true, temporary: true, relaxed: true},
		{name: "relaxed_temporary_zero_budget", budget: 0, temporary: true, relaxed: true},
		{name: "relaxed_temporary_visible", budget: 1, temporary: true, streamFailure: true, visible: true, relaxed: true},
		{name: "temporary_stream_sticky", budget: 1, temporary: true, streamFailure: true},
		{name: "native_temporary_stream_sticky", native: true, budget: 1, temporary: true, streamFailure: true},
		{name: "temporary_stream_budget_exhausted", budget: 1, temporary: true, streamFailure: true, temporaryAlways: true},
		{name: "native_temporary_stream_budget_exhausted", native: true, budget: 1, temporary: true, streamFailure: true, temporaryAlways: true},
		{name: "different_tags", budget: 1, targetMismatch: "tags"},
		{name: "native_different_tags", native: true, budget: 1, targetMismatch: "tags"},
		{name: "different_groups", budget: 1, targetMismatch: "groups"},
		{name: "different_model", budget: 1, targetMismatch: "model"},
		{name: "visible_output", streamFailure: true, visible: true, budget: 1},
		{name: "native_visible_output", native: true, streamFailure: true, visible: true, budget: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, owner, target, _ := failoverTestSetup(t, !tc.disabled)
			settings := CurrentRuntimeSettings()
			settings.CodexPreflightSSEPassthrough = true
			settings.CodexSessionFailoverPreserveInput = tc.preserve
			if tc.relaxed {
				settings.CodexForkAccountFallbackEnabled = true
				settings.CodexSessionFailoverEnabled = false
			}
			settings.CompactViaResponses = tc.compact && tc.streamFailure
			settings.CodexWSSilentRetry, settings.CodexWSSilentRetries = tc.budget > 0, tc.budget
			ApplyRuntimeSettings(settings)
			h.store.SetMaxRetries(0)
			h.store.SetMaxRateLimitRetries(tc.budget)
			h.store.SetRetryIntervalMS(1)
			h.store.SetTransportRetryPolicy("sticky")
			if tc.relaxed {
				h.store.SetTransportRetryPolicy("rotate")
			}
			switch tc.targetMismatch {
			case "tags":
				target.Tags = []string{"different"}
			case "groups":
				target.GroupIDs = []int64{999}
			case "model":
				target.Models = []string{"gpt-5.6-luna"}
			}
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			oldResin := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(oldResin) })
			type capture struct {
				headers http.Header
				body    []byte
			}
			seen := make(chan capture, 16)
			var failing atomic.Bool
			var failures atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				seen <- capture{r.Header.Clone(), body}
				if failing.Load() && (r.Header.Get("Authorization") == "Bearer owner-token" || tc.targetQuota) && (!tc.temporary || tc.temporaryAlways || failures.Load() == 0) {
					failures.Add(1)
					errorType := "usage_limit_reached"
					if tc.temporary {
						errorType = "rate_limit_exceeded"
					}
					failure := fmt.Sprintf(`{"type":%q,"code":%q,"message":"upstream quota test","resets_at":%d,"plan_type":"plus"}`, errorType, errorType, time.Now().Add(time.Hour).Unix())
					if tc.temporary {
						failure = `{"type":"rate_limit_exceeded","code":"rate_limit_exceeded","message":"too many requests; try again"}`
					}
					if tc.headerQuota {
						failure = `{"type":"rate_limit_exceeded","message":"too many requests"}`
						w.Header().Set("x-codex-primary-used-percent", "100")
						w.Header().Set("x-codex-primary-window-minutes", "300")
						w.Header().Set("x-codex-primary-reset-after-seconds", "3600")
					}
					if tc.streamFailure {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"codex.rate_limits\"}\n\n")
						if tc.visible {
							_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"already visible\"}\n\n")
							w.(http.Flusher).Flush()
						}
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":%s}}\n\n", failure)
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = fmt.Fprintf(w, `{"error":%s}`, failure)
					}
					return
				}
				real := "real-" + r.Header.Get("Authorization")
				w.Header().Set(codexTurnStateHeader, real)
				if strings.HasSuffix(r.URL.Path, "/compact") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"compact-quota","output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				payload, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{"x-codex-turn-state": real}})
				_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
				stickyFailureSuccess(w)
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "quota-retry"})
			root, turn := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
			_, body := failoverTestRequest(t, h)
			body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(root))
			body, _ = sjson.SetBytes(body, "stream", true)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", turn)
			body = addSessionTools(t, body)
			probe, _ := newTurnStateTestContext(t)
			probe.Set(contextAPIKeyID, int64(101))
			probe.Request.Header.Set("Authorization", "Bearer test-user-key")
			identity := h.resolveRequestSessionIdentityForContext(probe, body)
			key := capacityAwareSessionAffinityKey(identity, 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: root, NumberKnown: true, LastSeen: time.Now()})
			require.NoError(t, err)
			h.store.BindSessionAffinity(key, owner, "")
			var conn *websocket.Conn
			if tc.native {
				engine := gin.New()
				engine.GET("/v1/responses", func(c *gin.Context) { c.Set(contextAPIKeyID, int64(101)); h.ResponsesWebSocket(c) })
				server := httptest.NewServer(engine)
				t.Cleanup(server.Close)
				conn, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-user-key"}})
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
			}
			var currentAlias string
			send := func(body []byte, compact bool) (string, int) {
				if currentAlias != "" {
					body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", currentAlias)
				}
				if tc.native {
					body, _ = sjson.SetBytes(body, "type", "response.create")
					require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
					require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
					var output string
					for {
						_, event, err := conn.ReadMessage()
						require.NoError(t, err, output)
						output += string(event)
						switch gjson.GetBytes(event, "type").String() {
						case "response.metadata":
							currentAlias = gjson.GetBytes(event, "headers.x-codex-turn-state").String()
						case "response.completed":
							return output, 200
						case "response.failed", "error":
							return output, 429
						}
					}
				}
				r := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(r)
				c.Set(contextAPIKeyID, int64(101))
				endpoint := "/v1/responses"
				if compact {
					endpoint += "/compact"
				}
				c.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
				ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				c.Request.Header.Set("Authorization", "Bearer test-user-key")
				c.Request.Header.Set(codexTurnStateHeader, currentAlias)
				if compact {
					h.ResponsesCompact(c)
				} else {
					h.Responses(c)
				}
				if !compact && strings.Contains(r.Body.String(), `"type":"response.completed"`) {
					headers := r.Result().Header
					require.Equal(t, "v1-loose", headers.Get(upstreamTimingHeader))
					ms, err := strconv.ParseInt(headers.Get(upstreamFirstResponseHeader), 10, 64)
					require.NoError(t, err)
					attemptMS, err := strconv.ParseInt(headers.Get(upstreamAttemptFirstResponseHeader), 10, 64)
					require.NoError(t, err)
					require.GreaterOrEqual(t, ms, attemptMS)
				}
				if token := r.Header().Get(codexTurnStateHeader); token != "" {
					currentAlias = token
				}
				return r.Body.String(), r.Code
			}
			output, status := send(body, false)
			require.Equal(t, 200, status, output)
			require.True(t, database.ValidCodexTurnStateAlias(currentAlias))
			first := <-seen
			aliasA := currentAlias
			failing.Store(true)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.window_number", 1)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.window_id", root+":1")
			output, status = send(body, tc.compact)
			blocked := tc.budget == 0 || tc.disabled || tc.targetMismatch == "groups" || tc.targetMismatch == "model" || tc.visible
			expectSwitch := !blocked && (!tc.temporary || tc.relaxed)
			var attempts []capture
			for len(seen) > 0 {
				attempts = append(attempts, <-seen)
			}
			require.NotEmpty(t, attempts)
			require.Equal(t, "Bearer owner-token", attempts[0].headers.Get("Authorization"))
			if !tc.compact {
				require.Equal(t, "real-Bearer owner-token", gjson.GetBytes(attempts[0].body, "client_metadata.x-codex-turn-state").String())
			}
			if blocked {
				require.Len(t, attempts, 1, output)
				require.NotContains(t, output, "response.completed")
				if tc.targetMismatch != "" {
					require.Contains(t, output, "no_available_account")
				}
			} else if tc.targetQuota || tc.temporaryAlways {
				require.Len(t, attempts, 2, output)
				require.NotContains(t, output, "response.completed")
				if tc.targetQuota {
					require.Equal(t, "Bearer target-token", attempts[1].headers.Get("Authorization"))
					require.False(t, gjson.GetBytes(attempts[1].body, "client_metadata.x-codex-turn-state").Exists())
				} else {
					require.Equal(t, "Bearer owner-token", attempts[1].headers.Get("Authorization"))
					require.NotEmpty(t, owner.GetCooldownReason())
				}
			} else {
				require.Equal(t, 200, status, output)
				require.Len(t, attempts, 2, output)
				if expectSwitch {
					retried := attempts[1]
					require.Equal(t, "Bearer target-token", retried.headers.Get("Authorization"))
					require.Empty(t, retried.headers.Get(codexTurnStateHeader))
					require.False(t, gjson.GetBytes(retried.body, "client_metadata.x-codex-turn-state").Exists())
					require.NotEqual(t, first.headers.Get("Session-Id"), retried.headers.Get("Session-Id"))
					require.NotEqual(t, aliasA, currentAlias)
					if !tc.compact {
						assertSessionTools(t, retried.body)
						meta := gjson.Parse(gjson.GetBytes(retried.body, "client_metadata.x-codex-turn-metadata").String())
						require.EqualValues(t, 0, meta.Get("window_number").Int())
						require.NotEqual(t, turn, meta.Get("turn_id").String())
						if tc.preserve {
							require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(retried.body, "input").Raw)
						}
					}
				} else {
					require.Equal(t, "Bearer owner-token", attempts[1].headers.Get("Authorization"))
					require.Equal(t, aliasA, currentAlias)
				}
				// A later request restores the committed owner and its new real state.
				aliasB := currentAlias
				output, status = send(body, false)
				require.Equal(t, 200, status, output)
				next := <-seen
				expectedAuth := "Bearer owner-token"
				if expectSwitch {
					expectedAuth = "Bearer target-token"
				}
				require.Equal(t, expectedAuth, next.headers.Get("Authorization"))
				require.Equal(t, "real-"+expectedAuth, gjson.GetBytes(next.body, "client_metadata.x-codex-turn-state").String())
				require.Equal(t, aliasB, currentAlias)
			}
			record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.True(t, found)
			if expectSwitch {
				require.Equal(t, target.ID(), record.AccountID)
				require.EqualValues(t, 1, record.FailoverCount)
			} else {
				require.Equal(t, owner.ID(), record.AccountID)
				require.Zero(t, record.FailoverCount)
			}
		})
	}
}

func TestSessionQuotaRetryOnlyObservedQuotaCanMigrate(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, true)
			c, body := failoverTestRequest(t, h)
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			exclusions := newSessionRetryAccountExclusions(c, key, body)
			owner.UsagePercent7d, owner.UsagePercent7dValid, owner.PlanType, owner.Reset7dAt = 100, true, "free", time.Now().Add(time.Hour)
			if observed {
				exclusions.MarkHTTPFailure(owner.ID(), 429, []byte(`{"error":{"type":"usage_limit_reached"}}`), 0, 1)
			}
			ctx, blocked := h.prepareSessionQuotaRetry(c.Request.Context(), key, exclusions, auth.DispatchPolicyStandard)
			require.False(t, blocked)
			selected, _, handled := h.takeSessionAccountFailover(ctx, key, 0, exclusions.ForSelection(), nil, auth.DispatchPolicyStandard)
			require.Equal(t, observed, handled)
			if observed {
				require.Same(t, target, selected)
				h.store.Release(selected)
			} else {
				require.Nil(t, selected)
			}
		})
	}
}

func TestSessionQuotaRetryRejectsUnsafeContextAndSkipsRelay(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprint(relay), func(t *testing.T) {
			h, owner, _, key := failoverTestSetup(t, true)
			c, body := failoverTestRequest(t, h)
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			// The bound owner can consume this history, but nothing replayable
			// remains after removing account-private ciphertext for a new owner.
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"private-old-context"}]`))
			c.Set(apiRelaySessionExemptContextKey, relay)
			exclusions := newSessionRetryAccountExclusions(c, key, body)
			exclusions.MarkHTTPFailure(owner.ID(), 429, []byte(`{"error":{"type":"usage_limit_reached"}}`), 0, 1)
			owner.UsagePercent7d, owner.UsagePercent7dValid, owner.PlanType, owner.Reset7dAt = 100, true, "free", time.Now().Add(time.Hour)
			_, blocked := h.prepareSessionQuotaRetry(c.Request.Context(), key, exclusions, auth.DispatchPolicyStandard)
			require.Equal(t, !relay, blocked)
			require.Equal(t, !relay, sessionFailoverDispatchBlocked(c))
			if blocked {
				err := sessionFailoverUnavailableAPIError(c)
				require.Equal(t, "codex_session_failover_context_required", string(err.Code))
				require.NotContains(t, err.Message, "private-old-context")
				h.sendDispatchUnavailable(c, false, false)
				require.Equal(t, 400, c.Writer.Status())
				require.Equal(t, "false", c.Writer.Header().Get("X-Should-Retry"))
			}
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.Equal(t, owner.ID(), record.AccountID)
			require.Zero(t, record.FailoverCount)
		})
	}
}

func TestSessionQuotaRetryCanPrepareAnotherGeneration(t *testing.T) {
	h, owner, target, key := failoverTestSetup(t, true)
	c, body := failoverTestRequest(t, h)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	exclusions := newSessionRetryAccountExclusions(c, key, body)
	for generation, account := range []*auth.Account{owner, target} {
		if generation == 1 {
			h.store.AddAccount(&auth.Account{DBID: 1697, AccountID: "861373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "third-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}})
		}
		account.UsagePercent7d, account.UsagePercent7dValid, account.PlanType, account.Reset7dAt = 100, true, "free", time.Now().Add(time.Hour)
		exclusions.MarkHTTPFailure(account.ID(), 429, []byte(`{"error":{"type":"usage_limit_reached"}}`), 0, 2)
		ctx, blocked := h.prepareSessionQuotaRetry(c.Request.Context(), key, exclusions, auth.DispatchPolicyStandard)
		require.False(t, blocked)
		selected, _, handled := h.takeSessionAccountFailover(ctx, key, 0, exclusions.ForSelection(), nil, auth.DispatchPolicyStandard)
		require.True(t, handled)
		require.NotNil(t, selected)
		require.EqualValues(t, 1696+generation, selected.ID())
		h.store.Release(selected)
		record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
		require.NoError(t, err)
		require.EqualValues(t, generation+1, record.FailoverCount)
	}
}

func TestSessionQuotaRetryFirstRequestCommittedOwner(t *testing.T) {
	h, owner, target, _ := failoverTestSetup(t, true)
	c, body := failoverTestRequest(t, h)
	key := "fresh-quota-root::api-key:101"
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	require.Zero(t, continuityRequest(c).Diagnostic.OwnerAccount)
	require.Nil(t, h.commitSessionContinuity(c, owner))
	exclusions := newSessionRetryAccountExclusions(c, key, body)
	exclusions.MarkHTTPFailure(owner.ID(), 429, []byte(`{"error":{"type":"usage_limit_reached"}}`), 0, 1)
	owner.UsagePercent7d, owner.UsagePercent7dValid, owner.PlanType, owner.Reset7dAt = 100, true, "free", time.Now().Add(time.Hour)
	ctx, blocked := h.prepareSessionQuotaRetry(c.Request.Context(), key, exclusions, auth.DispatchPolicyStandard)
	require.False(t, blocked)
	selected, _, handled := h.takeSessionAccountFailover(ctx, key, 0, exclusions.ForSelection(), nil, auth.DispatchPolicyStandard)
	require.True(t, handled)
	require.Same(t, target, selected)
	h.store.Release(selected)
}
