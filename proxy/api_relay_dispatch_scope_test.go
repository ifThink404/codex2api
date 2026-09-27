package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestAPIAccountGroupAuthorizationDoesNotDependOnTransport(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, relaxed := range []bool{false, true} {
			for _, missingIdentity := range []bool{false, true} {
				name := map[bool]string{false: "compat", true: "raw"}[raw] + "/" + map[bool]string{false: "strict", true: "relaxed"}[relaxed] + "/" + map[bool]string{false: "complete", true: "missing"}[missingIdentity]
				t.Run(name, func(t *testing.T) {
					var apiCalls, nativeCalls atomic.Int32
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						apiCalls.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, modelQuotaSSE)
					}))
					defer up.Close()
					h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, true)
					row.AllowedGroupIDs = []int64{10}
					h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
					native := h.store.FindByID(1)
					native.GroupIDs = []int64{10}
					installClaudeBoundaryTransport(t, native, func(r *http.Request) (*http.Response, error) {
						nativeCalls.Add(1)
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(modelQuotaSSE)), Request: r}, nil
					})
					relay := addRawRelayTestAccount(h, up.URL)
					relay.GroupIDs = []int64{20}
					relay.OpenAIRawPassthrough = raw
					relay.SchedulerPriority = 100
					UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
					body := []byte(`{"model":"gpt-6-astra","input":"hello","stream":true,"reasoning":{"effort":"high"}}`)
					headers := nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
					if missingIdentity {
						body = []byte(`{"model":"gpt-6-astra","input":"hello","stream":true}`)
						headers = http.Header{}
					}
					c, w := rawRoutingTestContext(row, "/v1/responses", body, headers)
					h.Responses(c)
					require.Equal(t, 200, w.Code, w.Body.String())
					require.EqualValues(t, 1, nativeCalls.Load())
					require.Zero(t, apiCalls.Load(), "an API in another account group is not authorized by the Key's Codex group")
				})
			}
		}
	}
}

func TestAPIRelayCompatibilityScopeAndFailover(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		for _, scenario := range []string{"same_group", "unrestricted_key", "ungrouped_account", "other_group", "initial_429"} {
			t.Run(scenario+map[bool]string{false: "/strict", true: "/relaxed"}[relaxed], func(t *testing.T) {
				var calls []string
				var fail atomic.Bool
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					calls = append(calls, name)
					if name == "relay-test" && fail.Load() {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(429)
						io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"retry another API"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, modelQuotaSSE)
				}))
				defer up.Close()
				h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
				first := h.store.FindByID(1)
				first.GroupIDs = []int64{20}
				row.AllowedGroupIDs = []int64{20, 30}
				if scenario == "unrestricted_key" || scenario == "ungrouped_account" {
					row.AllowedGroupIDs = nil
				}
				if scenario == "ungrouped_account" {
					first.GroupIDs = nil
				}
				h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
				h.store.SetMaxRateLimitRetries(1)
				h.store.SetRetryIntervalMS(0)
				h.store.SetTransportRetryPolicy("rotate")
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				body := []byte(`{"model":"gpt-6-astra","input":"hello","stream":true,"reasoning":{"effort":"high"}}`)
				run := func() *httptest.ResponseRecorder {
					c, w := rawRoutingTestContext(row, "/v1/responses", body, nativeSessionHeaders(testRootSessionA, testRootSessionA, 0))
					h.Responses(c)
					return w
				}
				if scenario != "initial_429" {
					w := run()
					require.Equal(t, 200, w.Code, w.Body.String())
				}
				second := &auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "second", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{20}}
				if scenario == "other_group" {
					second.GroupIDs = []int64{30}
				}
				if scenario == "initial_429" {
					first.SchedulerPriority = 100
				}
				h.store.AddAccount(second)
				fail.Store(true)
				w := run()
				if relaxed || scenario == "same_group" || scenario == "unrestricted_key" || scenario == "initial_429" {
					require.Equal(t, 200, w.Code, w.Body.String())
					require.Equal(t, "second", calls[len(calls)-1])
				} else {
					require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
					for _, call := range calls {
						require.Equal(t, "relay-test", call, "API must not escape its configured cohort")
					}
				}
				for _, a := range h.store.Accounts() {
					require.Zero(t, a.ActiveRequests)
					require.Zero(t, a.OccupiedRequests)
				}
			})
		}
	}
}

func TestAPIRelayScopeNeverWidensAfterSelection(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	c, _, identity := chatGroupTestRequest(t, h, "/v1/responses")
	owner := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://api.invalid", APIKey: "test", GroupIDs: []int64{10}}
	next := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://api.invalid", APIKey: "test", GroupIDs: []int64{10}}
	other := &auth.Account{DBID: 3, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://api.invalid", APIKey: "test", GroupIDs: []int64{20}}
	native := &auth.Account{DBID: 4, GroupIDs: []int64{10}}
	filter := applyAffinityGroupRouting(c, identity, nil)
	rememberAPIRelayDispatchScope(c, owner)
	require.True(t, filter(owner))
	require.True(t, filter(next))
	require.False(t, filter(other))
	require.False(t, filter(native))
	rememberAPIRelayDispatchScope(c, other)
	require.False(t, filter(other), "a retry cannot overwrite the original route scope")
	owner.GroupIDs = []int64{20}
	require.False(t, filter(next), "changed membership must not retain old switch permission")
}
