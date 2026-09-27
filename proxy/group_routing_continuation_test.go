package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBoundSessionRetainsGroupWhenEffortChanges(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, relaxed := range []bool{false, true} {
		for _, startSplit := range []bool{false, true} {
			t.Run(map[bool]string{false: "strict", true: "relaxed"}[relaxed]+map[bool]string{false: "/primary_to_missing", true: "/split_to_complete"}[startSplit], func(t *testing.T) {
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				seen := []string{}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					seen = append(seen, r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, modelQuotaSSE)
				}))
				defer up.Close()
				h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, true)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				prev := GetResinConfig()
				t.Cleanup(func() { SetResinConfig(prev) })
				SetResinConfig(&ResinConfig{BaseURL: up.URL, PlatformName: "routing-audit"})
				t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
				configureRawRoutingTestGroups(h, row, false)
				split := &auth.Account{DBID: 2, GroupIDs: []int64{20}, AccessToken: "split-token", PlanType: "pro", Models: []string{"gpt-6-astra"}}
				primary := h.store.FindByID(1)
				require.NotNil(t, primary)
				if startSplit {
					h.store.AddAccount(split)
					atomic.StoreInt32(&primary.Disabled, 1)
				}
				id, err := uuid.NewV7()
				require.NoError(t, err)
				headers := nativeSessionHeaders(id.String(), id.String(), 0)
				for index, phase := range []string{"first", "continuation"} {
					body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello","reasoning":{"effort":"low"}}`)
					if index == 1 {
						atomic.StoreInt32(&primary.Disabled, 0)
						if !startSplit {
							h.store.AddAccount(split)
						}
					}
					if (index == 0 && startSplit) || (index == 1 && !startSplit) {
						body = []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello again"}`)
					}
					c, w := rawRoutingTestContext(row, "/v1/responses", body, headers)
					ctx, cancel := context.WithTimeout(c.Request.Context(), 1500*time.Millisecond)
					c.Request = c.Request.WithContext(ctx)
					h.Responses(c)
					cancel()
					require.Equal(t, 200, w.Code, w.Body.String())
					require.Len(t, seen, index+1)
					if index == 1 {
						require.Equal(t, seen[0], seen[1], "existing owner must survive effort presence changes")
					}
					result := map[string]any{"relaxed": relaxed, "runtime_relaxed": CurrentRuntimeSettings().CodexForkAccountFallbackEnabled, "start_split": startSplit, "phase": phase, "status": w.Code, "reason": usageRequestDiagnosticState(c).GroupRouting.Reason, "upstream_calls": len(seen), "rejections": selectionTraceForRequest(c).Snapshot().Reasons}
					raw, err := json.Marshal(result)
					require.NoError(t, err)
					t.Log("INGRESS " + string(raw))
				}
			})
		}
	}
}
