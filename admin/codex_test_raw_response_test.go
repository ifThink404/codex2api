package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise native and API relay execution with the actual diagnostic recorder;
// recorder-only tests miss response filtering performed by either executor.
func TestConnectionCodexPreservesRawDiagnosticResponse(t *testing.T) {
	for _, transport := range []string{"http", "websocket", "api_relay", "bps"} {
		for _, status := range []string{"completed", "failed"} {
			t.Run(transport+"_"+status, func(t *testing.T) {
				t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
				oldResin, oldSettings, oldExecutor := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings(), proxy.WebsocketExecuteFunc
				t.Cleanup(func() {
					wsrelay.ShutdownManager()
					proxy.SetResinConfig(oldResin)
					proxy.ApplyRuntimeSettings(oldSettings)
					proxy.WebsocketExecuteFunc = oldExecutor
				})
				settings := proxy.DefaultRuntimeSettings()
				settings.CodexForceWebsocket = transport == "websocket"
				proxy.ApplyRuntimeSettings(settings)
				proxy.WebsocketExecuteFunc = wsrelay.ExecuteRequestWebsocket
				const secret = "connection-test-access-token-123456"
				const state = "original-connection-test-turn-state"
				metadata := `{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + state + `","x-request-id":"original-request","cf-ray":"original-ray","set-cookie":"private-cookie","authorization":"Bearer ` + secret + `"}}`
				response := map[string]any{
					"id": "resp_original_diagnostic", "model": "gpt-5.5", "status": status,
					"session_id": "original-session", "metadata": map[string]any{"nested": map[string]any{"account_id": "original-account"}},
					"credential_echo": secret,
					"usage":           map[string]int{"input_tokens": 23, "output_tokens": 144},
				}
				if transport == "bps" {
					response["instructions"] = strings.Repeat("Basis Points private runtime name. ", 5000)
					response["tools"] = []any{map[string]any{"type": "function", "name": "request_user_input_basispoints"}}
					response["metadata"] = map[string]any{"bps_tools_version_id": "private-tools", "nested": map[string]any{"account_id": "original-account"}}
				}
				if status == "failed" {
					response["error"] = map[string]string{"type": "server_error", "code": "server_error", "message": "diagnostic failure"}
				}
				terminal, err := json.Marshal(map[string]any{"type": "response." + status, "response": response})
				require.NoError(t, err)
				frames := []string{metadata, string(terminal)}
				if status == "completed" {
					frames = []string{metadata, `{"type":"response.output_text.delta","delta":"OK"}`, string(terminal)}
				}
				type capturedRequest struct {
					headers http.Header
					body    []byte
				}
				received := make(chan capturedRequest, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if transport == "websocket" {
						conn, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						_, body, err := conn.ReadMessage()
						if err != nil {
							return
						}
						received <- capturedRequest{r.Header.Clone(), body}
						for _, frame := range frames {
							if conn.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
								return
							}
						}
						return
					}
					body, _ := io.ReadAll(r.Body)
					received <- capturedRequest{r.Header.Clone(), body}
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("X-Codex-Turn-State", state)
					w.Header().Set("X-Request-ID", "original-request")
					for _, frame := range frames {
						_, _ = io.WriteString(w, "data: "+frame+"\n\n")
					}
				}))
				t.Cleanup(server.Close)
				proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "raw-diagnostic-test"})
				store := auth.NewStore(nil, nil, &database.SystemSettings{TestModel: "gpt-5.5"})
				t.Cleanup(store.Stop)
				account := &auth.Account{DBID: 42, AccessToken: secret, AccountID: "selected-account", Status: auth.StatusReady}
				wantTransport := transport
				if transport == "bps" {
					account.CodexBPS = true
					wantTransport = "http"
				}
				if transport == "api_relay" {
					account = &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: secret, Status: auth.StatusReady, Models: []string{"gpt-5.5"}}
					wantTransport = "http"
				}
				store.AddAccount(account)
				events := decodeCodexTestEvents(t, serveCodexDiagnosticsTest(&Handler{store: store}).Body.String())
				require.GreaterOrEqual(t, len(events), 4)
				d := events[len(events)-1].CodexDiagnostics
				require.NotNil(t, d)
				require.Equal(t, "diagnostics", events[len(events)-1].Type)
				require.Equal(t, wantTransport, d.Transport)
				require.Equal(t, "resp_original_diagnostic", d.ResponseID)
				require.Equal(t, "original-request", d.RequestID)
				require.Equal(t, "original-ray", d.CFRay)
				require.Equal(t, status, d.ResponseStatus)
				require.NotNil(t, d.Usage)
				require.EqualValues(t, 23, *d.Usage.InputTokens)
				require.EqualValues(t, 144, *d.Usage.OutputTokens)
				headers := make(map[string]string)
				for _, header := range d.ResponseHeaders {
					headers[header.Name] = header.Value
				}
				require.Equal(t, state, headers["x-codex-turn-state"])
				require.NotNil(t, d.TurnStateLength)
				require.Equal(t, len(state), *d.TurnStateLength)
				require.NotContains(t, headers, "authorization")
				require.NotContains(t, headers, "set-cookie")
				for _, value := range []string{state, "resp_original_diagnostic"} {
					require.Contains(t, d.ResponseBody, value)
				}
				if transport == "bps" {
					require.NotContains(t, d.ResponseBody, "original-account")
					require.NotContains(t, d.ResponseBody, "private runtime name")
					require.False(t, d.BodyTruncated, "private provider prompt must be removed before capture")
					require.Equal(t, proxy.CodexBaseURL+"/responses", d.UpstreamEndpoint)
					encoded, err := json.Marshal(d)
					require.NoError(t, err)
					for _, marker := range []string{"bps_compat", "basispoints", "Basis Points", "\"mode\":\"bps\""} {
						require.NotContains(t, string(encoded), marker)
					}
					require.NotNil(t, d.Compatibility)
				} else {
					require.Contains(t, d.ResponseBody, "original-account")
				}
				// The existing diagnostic text redactor masks session values;
				// preserve that behavior rather than the user filter deleting fields.
				require.Contains(t, d.ResponseBody, `"session_id":"[REDACTED]"`)
				require.NotContains(t, d.ResponseBody, secret)
				require.Contains(t, d.ResponseBody, "[REDACTED]")
				require.Len(t, received, 1)
				sent := <-received
				outbound := sent.body
				if transport == "bps" {
					// The shared test builder supplies Codex URL metadata, but the
					// BPS projection must continue to use only its own protocol.
					require.False(t, gjson.GetBytes(outbound, "client_metadata").Exists())
					require.NotContains(t, string(outbound), "base_url")
					require.Empty(t, sent.headers.Get("X-Codex-Turn-Metadata"))
					require.NotEmpty(t, gjson.GetBytes(outbound, "metadata.task_id").String())
					require.Equal(t, "word", sent.headers.Get("X-Openai-Internal-Basispoints-Client-Editor"))
				}
				require.Equal(t, "gpt-5.5", gjson.GetBytes(outbound, "model").String())
				require.False(t, strings.Contains(string(outbound), state), "response opt-out must not copy upstream state into the test request")
			})
		}
	}
}
