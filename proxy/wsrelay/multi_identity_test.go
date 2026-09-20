package wsrelay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMultiIdentityHTTPCompactAndWebsocketWire(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	t.Setenv("CODEX_TELEMETRY_ENABLED", "false")
	previousResin, previousRuntime, previousExecutor := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings(), proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.WebsocketExecuteFunc = previousExecutor
	})
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexUserAgentConfig = `{"mode":"multi","profiles":{"codex-tui":{"client_version":"0.155.1"},"codex-desktop":{"client_version":"0.155.2","app_version":"26.915.31029"},"codex-vscode":{"client_version":"0.155.3","app_version":"26.915.31030"},"codex-exec":{"client_version":"0.155.4"},"custom":{"client_name":"Fallback Client","client_version":"0.155.5"}}}`
	proxy.ApplyRuntimeSettings(settings)
	type capture struct {
		headers http.Header
		body    []byte
	}
	captures := make(chan capture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			body, _ := io.ReadAll(r.Body)
			captures <- capture{r.Header.Clone(), body}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"test-response","output":[]}`))
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			captures <- capture{r.Header.Clone(), body}
			if conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"test-response","status":"completed","output":[]}}`)) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "multi-identity-test"})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	var usedConnections []*WsConnection
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		res, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		usedConnections = append(usedConnections, res.conn)
		return websocketResponseToHTTP(ctx, res, http.StatusOK, nil), nil
	}
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "multi.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 20*time.Second)
	defer cancel()
	account := &auth.Account{DBID: 1695, AccountID: "test-account", AccessToken: "test-token", CodexInstallationID: "account-device", CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	const root = "01a09302-49f4-7b53-b545-91ef29610317"
	body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"}],"client_metadata":{"x-codex-installation-id":"untrusted-device","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","installation_id":"untrusted-device"}}}`, root, root))
	clients := []struct{ incoming, outgoing, version string }{
		{"codex-tui", "codex-tui", "0.155.1"},
		{"Codex Desktop", "Codex Desktop", "0.155.2"},
		{"codex_vscode", "codex_vscode", "0.155.3"},
		{"codex_exec", "codex_exec", "0.155.4"},
		{"unknown", "Fallback Client", "0.155.5"},
		{"unknown", "Fallback Client", "0.155.5"},
	}
	for _, transport := range []string{"http", "compact", "ws"} {
		for _, client := range clients {
			headers := http.Header{"Session-Id": {root}, "Thread-Id": {root}, "User-Agent": {client.incoming + "/9.9.9 (untrusted-platform)"}, "X-Codex-Installation-Id": {"untrusted-device"}}
			var response *http.Response
			if transport == "compact" {
				response, err = proxy.ExecuteCompactRequest(ctx, account, body, "cache", "", "key", nil, headers)
			} else {
				response, err = proxy.ExecuteRequest(ctx, account, body, "cache", "", "key", nil, headers, transport == "ws")
			}
			require.NoError(t, err, "%s/%s", transport, client.incoming)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			var sent capture
			select {
			case sent = <-captures:
			case <-ctx.Done():
				t.Fatal("missing wire capture")
			}
			require.Contains(t, sent.headers.Get("User-Agent"), client.outgoing+"/"+client.version+" ", "%s", transport)
			require.Equal(t, client.outgoing, sent.headers.Get("Originator"))
			require.Equal(t, client.version, sent.headers.Get("Version"))
			require.Equal(t, "account-device", sent.headers.Get("X-Codex-Installation-Id"))
			require.NotContains(t, fmt.Sprint(sent.headers), "untrusted")
			require.NotContains(t, string(sent.body), "untrusted")
			if transport != "compact" {
				require.Equal(t, "account-device", gjson.GetBytes(sent.body, "client_metadata.x-codex-installation-id").String())
				meta := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata")
				if meta.Type == gjson.String {
					meta = gjson.Parse(meta.String())
				}
				require.Equal(t, "account-device", meta.Get("installation_id").String())
			}
		}
	}
	require.Len(t, usedConnections, 6)
	for i := 1; i < 5; i++ {
		require.NotSame(t, usedConnections[i-1], usedConnections[i], "client switch must not reuse a conflicting handshake")
	}
	require.Same(t, usedConnections[4], usedConnections[5], "same identity should retain connection reuse")
}
