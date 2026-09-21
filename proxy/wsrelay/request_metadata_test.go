package wsrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestWebsocketReusedConnectionUsesOnlyCurrentFrameMetadata(test *testing.T) {
	test.Setenv("CODEX_TELEMETRY_ENABLED", "false")
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "aligned")
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		for _, pooled := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/pooled=%t", mode, pooled), func(test *testing.T) {
				test.Setenv("CODEX_WS_STATELESS_ONESHOT", "false")
				test.Setenv("CODEX_SESSION_HEADER_MODE", "native")
				previousResin := proxy.GetResinConfig()
				previousRuntime := proxy.CurrentRuntimeSettings()
				previousExecutor := proxy.WebsocketExecuteFunc
				test.Cleanup(func() {
					proxy.SetResinConfig(previousResin)
					proxy.ApplyRuntimeSettings(previousRuntime)
					proxy.WebsocketExecuteFunc = previousExecutor
				})
				proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
				type capture struct {
					connection int64
					headers    http.Header
					body       []byte
				}
				received := make(chan capture, 4)
				var connections atomic.Int64
				upgrader := websocket.Upgrader{}
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					connection, err := upgrader.Upgrade(writer, request, nil)
					if err != nil {
						return
					}
					defer connection.Close()
					connectionID := connections.Add(1)
					for {
						_, payload, err := connection.ReadMessage()
						if err != nil {
							return
						}
						received <- capture{connectionID, request.Header.Clone(), payload}
						turn := gjson.GetBytes(payload, "client_metadata.x-codex-turn-metadata").String()
						responseID := "resp_" + gjson.Get(turn, "turn_id").String()
						if err := connection.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "status": "completed", "output": []any{}}}); err != nil {
							return
						}
					}
				}))
				test.Cleanup(server.Close)
				proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "metadata-test"})
				manager := NewManager()
				test.Cleanup(manager.Stop)
				executor := NewExecutorWithManager(manager)
				proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
					response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
					if err != nil {
						return nil, err
					}
					return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
				}
				account := &auth.Account{DBID: 902, AccountID: "fixture", AccessToken: "dummy-token", CodexFingerprintMode: mode, CodexInstallationID: "account-device", DynamicConcurrencyLimit: 1}
				account.CustomHeaders = map[string]string{"X-Codex-Project-Id": "custom-project", "X-Codex-Workspace-Id": "custom-workspace"}
				stale := make(http.Header)
				stale.Set("Originator", "codex_cli_rs")
				stale.Set("Thread-Id", "stale-thread")
				stale.Set("X-Codex-Window-Id", "stale-thread:0")
				stale.Set("X-Codex-Parent-Thread-Id", "stale-parent")
				stale.Set("X-OpenAI-Subagent", "stale-agent")
				stale.Set("X-OpenAI-Memgen-Request", "true")
				stale.Set("X-Codex-Turn-State", "stale-state")
				stale.Set("X-Codex-Turn-Metadata", `{"thread_id":"stale-thread","parent_thread_id":"stale-parent"}`)
				unchanged := stale.Clone()
				for turn := 0; turn < 3; turn++ {
					thread := "current-thread"
					if pooled {
						thread += strconv.Itoa(turn)
					}
					canonical := map[string]any{
						"installation_id": "device", "session_id": "root", "thread_id": thread,
						"window_id": fmt.Sprintf("%s:%d", thread, turn), "window_number": turn,
						"context_window_id": "context-" + strconv.Itoa(turn), "turn_id": strconv.Itoa(turn),
						"request_kind": "turn", "thread_source": "user",
						"project_id": "project", "projectId": "project", "workspace_id": "workspace",
					}
					if pooled && turn == 0 {
						canonical["parent_thread_id"] = "root"
						canonical["forked_from_thread_id"] = "fork-source"
						canonical["subagent_kind"] = "review"
						canonical["thread_source"] = "subagent"
						canonical["request_kind"] = "memory"
					}
					raw, _ := json.Marshal(canonical)
					body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "client_metadata": map[string]any{"x-codex-turn-metadata": string(raw), "project_id": "flat-project", "projectId": "flat-project", "workspace_id": "flat-workspace"}})
					sessionID := "gateway-cache"
					if pooled {
						sessionID = ""
					}
					if !pooled && turn > 0 {
						body, _ = json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "previous_response_id": "resp_" + strconv.Itoa(turn-1), "client_metadata": map[string]any{"x-codex-turn-metadata": string(raw), "project_id": "flat-project", "projectId": "flat-project", "workspace_id": "flat-workspace"}})
					}
					expected := proxy.NewCodexTransportFingerprint(account, stale, body, sessionID).ApplyBody(body)
					expectedMetadata, _ := sjson.Set(gjson.GetBytes(expected, codexTurnMetadataClientPath).String(), "analytics_enabled", false)
					expectedMetadata, _ = sjson.Set(expectedMetadata, "installation_id", "account-device")
					memoryExtraction := pooled && turn == 0
					if memoryExtraction {
						for _, field := range []string{"installation_id", "session_id", "thread_id", "agent_name", "window_id", "window_number", "context_window_id"} {
							expectedMetadata, _ = sjson.Delete(expectedMetadata, field)
						}
					}
					expected, _ = sjson.SetBytes(expected, codexTurnMetadataClientPath, expectedMetadata)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					response, err := proxy.ExecuteRequest(ctx, account, body, sessionID, "", "same-key", nil, stale, true)
					if err != nil {
						test.Fatal(err)
					}
					_, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr != nil {
						test.Fatal(readErr)
					}
					var sent capture
					select {
					case sent = <-received:
					case <-ctx.Done():
						test.Fatal("mock did not receive frame")
					}
					expectedConnection := int64(1)
					if pooled {
						expectedConnection = int64(turn + 1)
					}
					if sent.connection != expectedConnection {
						test.Fatalf("unexpected connection reuse: %d, want %d", sent.connection, expectedConnection)
					}
					for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Project-Id", "X-Codex-Workspace-Id"} {
						if sent.headers.Get(name) != "" {
							test.Fatalf("frozen handshake contains per-request field %s", name)
						}
					}
					metadata := gjson.GetBytes(sent.body, codexTurnMetadataClientPath).String()
					if !pooled && turn > 0 && gjson.GetBytes(sent.body, "previous_response_id").String() != "resp_"+strconv.Itoa(turn-1) {
						test.Fatal("existing response continuation was rewritten or removed")
					}
					if sent.headers.Get("Session-Id") != "root" || gjson.GetBytes(sent.body, "client_metadata.session_id").String() != "root" || !memoryExtraction && gjson.Get(metadata, "session_id").String() != "root" || sent.headers.Get("Thread-Id") != thread {
						test.Fatal("reused WS frame session differs from handshake")
					}
					if turn > 0 && (sent.headers.Get("X-OpenAI-Memgen-Request") != "" || sent.headers.Get("X-OpenAI-Subagent") != "" || sent.headers.Get("X-Codex-Parent-Thread-Id") != "") {
						test.Fatal("task identity inherited from an incompatible connection")
					}
					for _, field := range []string{"project_id", "projectId", "workspace_id"} {
						if gjson.Get(metadata, field).Exists() || gjson.GetBytes(sent.body, "client_metadata."+field).Exists() {
							test.Fatalf("project metadata survived in WS frame: %s", sent.body)
						}
					}
					if metadata != gjson.GetBytes(expected, codexTurnMetadataClientPath).String() {
						test.Fatalf("current canonical metadata changed or rehashed: %s, want %s", metadata, expected)
					}
					if window := gjson.GetBytes(sent.body, "client_metadata.x-codex-window-id").String(); window != fmt.Sprintf("%s:%d", thread, turn) || !memoryExtraction && window != gjson.Get(metadata, "window_id").String() {
						test.Fatalf("window projection differs: %s", sent.body)
					}
					if parent := gjson.GetBytes(sent.body, "client_metadata.x-codex-parent-thread-id").String(); parent != gjson.Get(metadata, "parent_thread_id").String() {
						test.Fatalf("parent projection differs: %s", sent.body)
					}
					if turn > 0 && (gjson.GetBytes(sent.body, "client_metadata.x-openai-subagent").Exists() || gjson.GetBytes(sent.body, "client_metadata.x-openai-memgen-request").Exists() || gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-state").Exists()) {
						test.Fatalf("absent optional fields inherited from handshake: %s", sent.body)
					}
				}
				if !reflect.DeepEqual(stale, unchanged) {
					test.Fatal("downstream handshake headers mutated")
				}
			})
		}
	}
}

func TestWebsocketProjectMetadataStrippedFromHeaderOnlyRequest(test *testing.T) {
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		test.Run(mode, func(test *testing.T) {
			metadata := `{"thread_id":"thread","window_id":"thread:71","project_id":"project","projectId":"project","workspace_id":"workspace"}`
			incoming := make(http.Header)
			incoming.Set("X-Codex-Turn-Metadata", metadata)
			account := &auth.Account{DBID: 903, CodexFingerprintMode: mode, CustomHeaders: map[string]string{
				"X-Codex-Turn-Metadata": metadata, "X-Codex-Project-Id": "project", "X-Codex-Workspace-Id": "workspace", "OpenAI-Project": "api-project",
			}}
			executor := &Executor{}
			body := []byte(`{"model":"gpt-6-astra"}`)
			headers := executor.prepareWebsocketHeaders("dummy-token", account, "account", "session", "key", nil, incoming, body)
			frame := applyCodexFrameMetadata(body, headers)
			for _, field := range []string{"project_id", "projectId", "workspace_id"} {
				if gjson.Get(headers.Get("X-Codex-Turn-Metadata"), field).Exists() || gjson.Get(gjson.GetBytes(frame, codexTurnMetadataClientPath).String(), field).Exists() {
					test.Fatalf("header-only project metadata survived: %s", frame)
				}
			}
			if headers.Get("X-Codex-Project-Id") != "" || headers.Get("X-Codex-Workspace-Id") != "" || headers.Get("OpenAI-Project") != "api-project" {
				test.Fatal("project header cleanup crossed API project boundary")
			}
			if incoming.Get("X-Codex-Turn-Metadata") != metadata {
				test.Fatal("incoming metadata changed")
			}
		})
	}
}

func TestResponseConnectionBindingChecksRequestScope(test *testing.T) {
	manager := NewManager()
	test.Cleanup(manager.Stop)
	session := NewSession(42, manager)
	session.SetConnected(true)
	connection := &WsConnection{session: session, PoolKey: "fixture-pool"}
	connection.SetState(StateConnected)
	connection.Touch()
	manager.connections.Store(connection.PoolKey, connection)
	manager.BindResponseConn("response", connection, "thread#ovf-1", 42, "key", "thread")
	if found, _ := manager.lookupResponseConn("response", 42, "key", "thread"); found != connection {
		test.Fatal("same-scope overflow connection lost")
	}
	for _, request := range []struct {
		account    int64
		key, scope string
	}{{43, "key", "thread"}, {42, "other-key", "thread"}, {42, "key", "other-thread"}, {42, "key", "stateless:pool"}} {
		if found, _ := manager.lookupResponseConn("response", request.account, request.key, request.scope); found != nil {
			test.Fatalf("connection crossed request scope: %+v", request)
		}
	}
}
