package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestDispatchRecoveryRawRetryOpaqueReferences(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		opaque           bool
	}{
		{"plain_chat", "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}]}`, false},
		{"inline_image", "/v1/messages", `{"model":"gpt-6-astra","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}`, false},
		{"tool_arguments_are_not_handles", "/v1/messages", `{"model":"gpt-6-astra","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"lookup","input":{"file_id":"a-user-argument"}}]}]}`, false},
		{"nested_tool_result_file", "/v1/messages", `{"model":"gpt-6-astra","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"document","source":{"type":"file","file_id":"file-owner-only"}}]}]}]}`, true},
		{"signed_thinking", "/v1/messages", `{"model":"gpt-6-astra","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning","signature":"signed-owner"}]}]}`, true},
		{"redacted_thinking", "/v1/messages", `{"model":"gpt-6-astra","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`, true},
		{"plain_responses", "/v1/responses", `{"model":"gpt-6-astra","input":"hello"}`, false},
		{"responses_file", "/v1/responses", `{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-owner-only"}]}]}`, true},
		{"chat_file", "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-owner-only"}}]}]}`, true},
		{"messages_file", "/v1/messages", `{"model":"gpt-6-astra","max_tokens":32,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file-owner-only"}}]}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received, err := io.ReadAll(r.Body)
				if err != nil || string(received) != tc.body {
					t.Errorf("raw body changed or unreadable")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") == "Bearer healthy" {
					secondCalls.Add(1)
					_, _ = io.WriteString(w, `{"id":"resp-audit","object":"response","status":"completed","output":[]}`)
				} else {
					firstCalls.Add(1)
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"429: Rate limit exceeded"}}`)
				}
			}))
			defer upstream.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, upstream.URL, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			row.AllowedGroupIDs = []int64{20}
			h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
			first := addRawRelayTestAccount(h, upstream.URL)
			first.GroupIDs, first.SchedulerPriority = []int64{20}, 100
			h.store.AddAccount(&auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "healthy", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{20}, OpenAIRawPassthrough: true})
			h.store.SetMaxRetries(0)
			h.store.SetMaxRateLimitRetries(2)
			h.store.SetRetryIntervalMS(1)
			c, w := rawRoutingTestContext(row, tc.path, []byte(tc.body), nil)
			require.True(t, h.tryRawRelay(c))
			require.EqualValues(t, 1, firstCalls.Load())
			dispatchRecoveryLog(t, map[string]any{"scenario": tc.name, "status": w.Code, "first_calls": firstCalls.Load(), "replacement_calls": secondCalls.Load(), "account_scoped_reference": tc.opaque})
			if tc.opaque {
				require.Zero(t, secondCalls.Load(), "account-scoped file handles must not be replayed to another credential")
			} else {
				require.Equal(t, 200, w.Code, "plain raw requests should honor the configured 429 retry budget")
			}
		})
	}
}
