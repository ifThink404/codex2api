package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestRelaxedRelayRetryAcrossGroups(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, relaxed := range []bool{false, true} {
			for _, ungrouped := range []bool{false, true} {
				name := map[bool]string{false: "compat", true: "raw"}[raw] + "/" + map[bool]string{false: "strict", true: "relaxed"}[relaxed] + "/" + map[bool]string{false: "cross_group", true: "ungrouped"}[ungrouped]
				t.Run(name, func(t *testing.T) {
					body := []byte(`{"model":"gpt-6-astra","input":"hello","stream":true,"reasoning":{"effort":"high"}}`)
					var seen []string
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
						seen = append(seen, credential)
						if raw {
							got, _ := io.ReadAll(r.Body)
							if !bytes.Equal(body, got) {
								t.Error("raw failover changed request bytes")
							}
						}
						if credential == "relay-test" {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusTooManyRequests)
							io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"retry another account"}}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, modelQuotaSSE)
					}))
					defer up.Close()
					h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
					owner := h.store.FindByID(1)
					owner.GroupIDs, owner.SchedulerPriority, owner.OpenAIRawPassthrough = []int64{10}, 100, raw
					row.AllowedGroupIDs = []int64{10}
					row.Limits.NoAffinityGroupIDs = []int64{20}
					if ungrouped {
						row.AllowedGroupIDs, row.Limits.NoAffinityGroupIDs, owner.GroupIDs = nil, nil, nil
					}
					h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
					h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
					h.store.SetMaxRetries(0)
					h.store.SetMaxRateLimitRetries(1)
					h.store.SetRetryIntervalMS(0)
					h.store.SetTransportRetryPolicy("rotate")
					h.store.AddAccount(&auth.Account{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "healthy", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{20}, OpenAIRawPassthrough: raw})
					UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
					c, w := rawRoutingTestContext(row, "/v1/responses", body, nativeSessionHeaders(testRootSessionA, testRootSessionA, 0))
					h.Responses(c)
					if relaxed {
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						require.Equal(t, []string{"relay-test", "healthy"}, seen)
					} else {
						require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
						require.Equal(t, []string{"relay-test"}, seen)
					}
					for _, account := range h.store.Accounts() {
						require.Zero(t, account.ActiveRequests)
						require.Zero(t, account.OccupiedRequests)
					}
				})
			}
		}
	}
}
