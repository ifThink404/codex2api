package proxy

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/stretchr/testify/require"
)

func TestRawRelayUnavailableOwnerAndModelChangeStayScoped(t *testing.T) {
	for _, scenario := range []string{"disabled", "model_change", "transport", "ungrouped_account", "no_model_candidate"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, modelQuotaSSE)
			}))
			defer up.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, true)
			configureRawRoutingTestGroups(h, row, false)
			first := addRawRelayTestAccount(h, up.URL)
			first.GroupIDs = []int64{20}
			second := &auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "second", PlanType: "api", Models: []string{"gpt-6-astra", "vendor/new"}, GroupIDs: []int64{20}, OpenAIRawPassthrough: true}
			h.store.AddAccount(second)
			key := sessionAffinityKey("raw-api:"+testRootSessionA, row.ID)
			h.store.BindSessionAffinity(key, first, "")
			model := "gpt-6-astra"
			switch scenario {
			case "disabled":
				first.Disabled = 1
			case "model_change":
				model = "vendor/new"
			case "ungrouped_account":
				row.AllowedGroupIDs = nil
				row.Limits.NoAffinityGroupIDs = nil
				h.store.SetAPIKeyAllowedGroups(row.ID, nil)
				h.store.SetAPIKeyNoAffinityGroups(row.ID, nil)
				first.GroupIDs = nil
				first.Disabled = 1
			case "no_model_candidate":
				model = "unsupported"
			case "transport":
				dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				first.BaseURL = dead.URL
				dead.Close()
				h.store.SetMaxRetries(1)
			}
			body := []byte(`{"model":"` + model + `","stream":true,"input":"hello"}`)
			c, w := rawRoutingTestContext(row, "/v1/responses", body, nativeSessionHeaders(testRootSessionA, testRootSessionA, 0))
			require.True(t, h.tryRawRelay(c))
			if scenario == "ungrouped_account" || scenario == "no_model_candidate" {
				require.Equal(t, 503, w.Code, w.Body.String())
				require.Zero(t, calls.Load())
			} else {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.EqualValues(t, 1, calls.Load())
				owner, ok := h.store.LiveSessionAccountID(key, time.Now())
				require.True(t, ok)
				require.EqualValues(t, 14, owner)
			}
			for _, a := range h.store.Accounts() {
				require.Zero(t, a.ActiveRequests)
				require.Zero(t, a.OccupiedRequests)
			}
		})
	}
}

func TestRawRelayFlushesBeforeCompletionAndDoesNotReplayStartedStream(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[failed], func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			defer close(release)
			terminal := rawCompletedEvent
			if failed {
				terminal = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"429 limited\"}}}\n\n"
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, terminal)
			}))
			defer up.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
			row.AllowedGroupIDs = []int64{20}
			h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
			first := addRawRelayTestAccount(h, up.URL)
			first.GroupIDs = []int64{20}
			h.store.AddAccount(&auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "second", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{20}, OpenAIRawPassthrough: true})
			h.store.SetMaxRetries(5)
			h.store.SetMaxRateLimitRetries(5)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
				s.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
				return s
			})
			engine := gin.New()
			engine.POST("/v1/responses", func(c *gin.Context) { c.Set(contextAPIKeyID, row.ID); c.Set(contextAPIKeyRow, row); h.Responses(c) })
			gateway := httptest.NewServer(engine)
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"hello","stream":true}`))
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			require.Contains(t, line, "first")
			release <- struct{}{}
			tail, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Contains(t, string(tail), terminal)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestRawRelayDoesNotPreemptNativeRoute(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		for _, tc := range []struct {
			name                                                      string
			sameGroup, bound, disabled, missingEffort, missingSession bool
		}{
			{name: "primary_codex_split_api"},
			{name: "same_group", sameGroup: true},
			{name: "bound_same_group", sameGroup: true, bound: true},
			{name: "bound_split_group", bound: true},
			{name: "disabled_split_api", disabled: true},
			{name: "missing_effort_uses_split", missingEffort: true},
			{name: "missing_session_uses_split", missingSession: true},
		} {
			t.Run(tc.name+map[bool]string{false: "/strict", true: "/relaxed"}[relaxed], func(t *testing.T) {
				t.Setenv("CODEX_TRANSPORT_MODE", "standard")
				body := []byte(`{"model":"gpt-6-astra","stream":true,"reasoning":{"effort":"high"},"input":"hello","vendor":9007199254740993}`)
				if tc.missingEffort {
					body = bytes.Replace(body, []byte(`,"reasoning":{"effort":"high"}`), nil, 1)
				}
				var rawCalls, nativeCalls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					rawCalls.Add(1)
					got, _ := io.ReadAll(r.Body)
					if !bytes.Equal(body, got) {
						t.Error("raw request changed")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, modelQuotaSSE)
				}))
				defer up.Close()
				h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, true)
				configureRawRoutingTestGroups(h, row, false)
				native := h.store.FindByID(1)
				installClaudeBoundaryTransport(t, native, func(r *http.Request) (*http.Response, error) {
					nativeCalls.Add(1)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(modelQuotaSSE)), Request: r}, nil
				})
				raw := addRawRelayTestAccount(h, up.URL)
				raw.GroupIDs = []int64{20}
				if tc.sameGroup {
					raw.GroupIDs = []int64{10}
				}
				if tc.disabled {
					raw.Disabled = 1
				}
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				if tc.bound {
					key := sessionAffinityKey(testRootSessionA, row.ID)
					h.store.BindSessionAffinity(key, native, "")
					_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: 1, ThreadID: testRootSessionA, UpstreamMode: "native", LastSeen: time.Now()})
					require.NoError(t, err)
				}
				headers := nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
				if tc.missingSession {
					headers = http.Header{}
				}
				c, w := rawRoutingTestContext(row, "/v1/responses", body, headers)
				h.Responses(c)
				require.Equal(t, 200, w.Code, w.Body.String())
				if !relaxed && (tc.missingEffort || tc.missingSession) {
					require.EqualValues(t, 1, rawCalls.Load())
					require.Zero(t, nativeCalls.Load())
				} else {
					require.EqualValues(t, 1, nativeCalls.Load())
					require.Zero(t, rawCalls.Load())
				}
			})
		}
	}
}

func TestRawRelayScopedFailoverAndSubsequentAffinity(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		for _, status := range []int{429, 500, 502} {
			t.Run(path+"/"+http.StatusText(status), func(t *testing.T) {
				body := []byte(` {"model":"gpt-6-astra","stream":true,"input":"hello","messages":[],"client_metadata":{"session_id":"` + testRootSessionA + `"},"vendor":9007199254740993} `)
				var fail atomic.Bool
				var seen []string
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					seen = append(seen, name)
					got, _ := io.ReadAll(r.Body)
					if !bytes.Equal(got, body) || r.Header.Get("Session-Id") != testRootSessionA {
						t.Error("switch modified raw identity/body")
					}
					if name == "first" && fail.Load() {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, modelQuotaSSE)
				}))
				defer up.Close()
				h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
				h.store.FindByID(1).Disabled = 1
				configureRawRoutingTestGroups(h, row, false)
				first := addRawRelayTestAccount(h, up.URL)
				first.GroupIDs = []int64{20}
				first.APIKey = "first"
				if status == 500 {
					// Old compatibility bindings may still exist when raw mode is
					// enabled. They must not undo a successful raw account switch.
					h.store.BindSessionAffinity(sessionAffinityKey(testRootSessionA, row.ID), first, "")
				}
				h.store.SetMaxRetries(2)
				h.store.SetMaxRateLimitRetries(2)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
				h.db.SetUsageLogConfig(database.UsageLogModeFull, 100, 60)
				run := func() {
					c, w := rawRoutingTestContext(row, path, body, nativeSessionHeaders(testRootSessionA, testRootSessionA, 0))
					require.True(t, h.tryRawRelay(c))
					require.Equal(t, 200, w.Code, w.Body.String())
					require.Equal(t, modelQuotaSSE, w.Body.String())
				}
				run() // Establish an owner before a second account is available.
				second := &auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "second", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{20}, OpenAIRawPassthrough: true}
				h.store.AddAccount(second)
				h.store.AddAccount(&auth.Account{DBID: 15, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "outside", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{30}, OpenAIRawPassthrough: true})
				fail.Store(true)
				run()
				run()
				require.Equal(t, []string{"first", "first", "second", "second"}, seen)
				usage, err := h.db.GetAPIKeyModelRequestUsage(t.Context(), row.ID, row.Limits.ModelRequestLimits, time.Now())
				require.NoError(t, err)
				require.Len(t, usage, 1)
				require.EqualValues(t, 3, usage[0].Used, "account retries must not consume a second client request quota")
				for _, a := range h.store.Accounts() {
					require.Zero(t, a.ActiveRequests)
					require.Zero(t, a.OccupiedRequests)
				}
				h.db.FlushUsageLogs()
				require.Eventually(t, func() bool {
					logs, err := h.db.ListUsageLogsByTimeRange(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
					if err != nil || len(logs) != 4 {
						return false
					}
					failures, retries := 0, 0
					for _, l := range logs {
						if l.StatusCode == status {
							failures++
						}
						if l.IsRetryAttempt {
							retries++
							require.Equal(t, 2, l.AttemptIndex)
							require.EqualValues(t, 14, l.AccountID)
						}
					}
					require.Equal(t, 1, failures)
					require.Equal(t, 1, retries)
					return true
				}, 2*time.Second, 10*time.Millisecond)
			})
		}
	}
}

func TestRawRelayFailoverCannotEscapeConfiguredScope(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		for _, scenario := range []string{"unrestricted_key_same_group", "no_source_group", "other_group", "unauthorized_key", "model_mismatch", "all_failed", "disabled_retries", "continuation", "encrypted_context", "permission_revoked", "group_changed"} {
			t.Run(scenario+map[bool]string{false: "/strict", true: "/relaxed"}[relaxed], func(t *testing.T) {
				body := []byte(`{"model":"gpt-6-astra","input":"hello","vendor":9007199254740993}`)
				if scenario == "continuation" {
					body = bytes.Replace(body, []byte(`"input":"hello"`), []byte(`"input":"hello","previous_response_id":"resp_original"`), 1)
				}
				if scenario == "encrypted_context" {
					body = bytes.Replace(body, []byte(`"input":"hello"`), []byte(`"input":[{"type":"reasoning","encrypted_content":"opaque"}]`), 1)
				}
				const failure = " {\"error\":{\"message\":\"keep-original-error\"},\"vendor\":9007199254740993}\n"
				var seen []string
				var h *Handler
				var row *database.APIKeyRow
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen = append(seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
					if scenario == "permission_revoked" {
						h.store.SetAPIKeyAllowedGroups(row.ID, []int64{99})
						h.store.SetAPIKeyNoAffinityGroups(row.ID, nil)
					}
					if scenario == "group_changed" {
						h.store.ApplyAccountGroups(14, []int64{30})
					}
					w.Header().Set("Retry-After", "17")
					w.Header().Set("X-Vendor", "unchanged")
					w.WriteHeader(429)
					io.WriteString(w, failure)
				}))
				defer up.Close()
				h, row, _ = newModelQuotaTestHandler(t, 100, up.URL, false)
				h.store.FindByID(1).Disabled = 1
				row.AllowedGroupIDs = []int64{20, 30}
				h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
				first := addRawRelayTestAccount(h, up.URL)
				first.GroupIDs = []int64{20}
				first.APIKey = "first"
				second := &auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "second", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{20}, OpenAIRawPassthrough: true}
				switch scenario {
				case "unrestricted_key_same_group":
					row.AllowedGroupIDs = nil
					h.store.SetAPIKeyAllowedGroups(row.ID, nil)
				case "no_source_group":
					row.AllowedGroupIDs = nil
					h.store.SetAPIKeyAllowedGroups(row.ID, nil)
					first.GroupIDs = nil
				case "other_group":
					second.GroupIDs = []int64{30}
				case "unauthorized_key":
					second.AllowedAPIKeyIDs = []int64{999}
				case "model_mismatch":
					second.Models = []string{"other-model"}
				}
				h.store.AddAccount(second)
				h.store.SetMaxRetries(5)
				h.store.SetMaxRateLimitRetries(5)
				if scenario == "disabled_retries" {
					h.store.SetMaxRateLimitRetries(0)
				}
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				key := sessionAffinityKey("raw-api:"+testRootSessionA, row.ID)
				h.store.BindSessionAffinity(key, first, "")
				c, w := rawRoutingTestContext(row, "/v1/responses", body, nativeSessionHeaders(testRootSessionA, testRootSessionA, 0))
				require.True(t, h.tryRawRelay(c))
				require.Equal(t, 429, w.Code, w.Body.String())
				require.Equal(t, failure, w.Body.String())
				require.Equal(t, "17", w.Header().Get("Retry-After"))
				require.Equal(t, "unchanged", w.Header().Get("X-Vendor"))
				if scenario == "all_failed" || scenario == "unrestricted_key_same_group" || relaxed && (scenario == "no_source_group" || scenario == "other_group" || scenario == "group_changed") {
					require.Equal(t, []string{"first", "second"}, seen)
				} else {
					require.Equal(t, []string{"first"}, seen)
				}
				for _, a := range h.store.Accounts() {
					require.Zero(t, a.ActiveRequests)
					require.Zero(t, a.OccupiedRequests)
				}
			})
		}
	}
}
