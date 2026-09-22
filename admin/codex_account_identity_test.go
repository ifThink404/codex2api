package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexAccountIdentitySingleAndBatchTests(test *testing.T) {
	for _, mode := range []string{"preserve", "account"} {
		test.Run(mode, func(test *testing.T) { testCodexAccountIdentitySingleAndBatchTests(test, mode, false) })
	}
	test.Run("account_websocket", func(test *testing.T) { testCodexAccountIdentitySingleAndBatchTests(test, "account", true) })
}

func testCodexAccountIdentitySingleAndBatchTests(test *testing.T, mode string, useWS bool) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", mode)
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	previousResin, previousSettings := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings()
	test.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.ApplyRuntimeSettings(previousSettings)
	})
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	if useWS {
		previousExecutor := proxy.WebsocketExecuteFunc
		proxy.WebsocketExecuteFunc = wsrelay.ExecuteRequestWebsocket
		proxy.UpdateRuntimeSettings(func(settings proxy.RuntimeSettings) proxy.RuntimeSettings {
			settings.CodexForceWebsocket = true
			settings.CodexWSContextTakeover = true
			return settings
		})
		test.Cleanup(func() { proxy.WebsocketExecuteFunc = previousExecutor })
		test.Cleanup(wsrelay.ShutdownManager)
	}
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if useWS && websocket.IsWebSocketUpgrade(request) {
			conn, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(writer, request, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				_, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				received <- capture{request.Header.Clone(), body}
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
					return
				}
			}
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(request.Body)
		received <- capture{request.Header.Clone(), body}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"test-response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "account-test"})
	dbPath := filepath.Join(test.TempDir(), "tests.db")
	db, err := database.New("sqlite", dbPath)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	store := auth.NewStore(nil, nil, &database.SystemSettings{TestModel: "gpt-5.5"})
	test.Cleanup(store.Stop)
	account := &auth.Account{DBID: 42, AccessToken: "test-token", AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", Status: auth.StatusReady}
	store.AddAccount(account)
	handler := &Handler{store: store, db: db}
	response := serveCodexDiagnosticsTest(handler)
	require.Equal(test, http.StatusOK, response.Code)
	require.Contains(test, response.Body.String(), `"success":true`)
	require.NoError(test, db.Close())
	db, err = database.New("sqlite", dbPath)
	require.NoError(test, err)
	handler = &Handler{store: store, db: db}
	account = &auth.Account{DBID: 42, AccessToken: "refreshed-token", AccountID: account.AccountID, Status: auth.StatusReady}
	status, message := handler.runSingleBatchTest(context.Background(), account)
	require.Equal(test, "success", status, message)
	status, message = handler.runRecycleBinSingleTest(context.Background(), account)
	require.Equal(test, "success", status, message)
	require.Len(test, received, 3)
	var previousSession, previousCache string
	threads, turns := make(map[string]bool), make(map[string]bool)
	for range 3 {
		sent := <-received
		session := sent.headers.Get("Session-Id")
		require.NotEmpty(test, session)
		cache := gjson.GetBytes(sent.body, "prompt_cache_key").String()
		if previousSession != "" {
			require.Equal(test, previousSession, session)
			require.Equal(test, previousCache, cache)
		}
		previousSession = session
		previousCache = cache
		thread := sent.headers.Get("Thread-Id")
		require.NotEmpty(test, thread)
		require.NotEqual(test, session, thread)
		require.False(test, threads[thread])
		threads[thread] = true
		turn := gjson.GetBytes(sent.body, "client_metadata.turn_id").String()
		require.NotEmpty(test, turn)
		require.False(test, turns[turn])
		turns[turn] = true
		require.Equal(test, session, sent.headers.Get("X-Codex-Parent-Thread-Id"))
		if useWS {
			require.Empty(test, sent.headers.Get("X-Codex-Window-Id"))
		} else {
			require.Equal(test, thread+":0", sent.headers.Get("X-Codex-Window-Id"))
		}
		require.Equal(test, "collab_spawn", sent.headers.Get("X-OpenAI-Subagent"))
		require.Equal(test, "medium", gjson.GetBytes(sent.body, "reasoning.effort").String())
		require.Empty(test, gjson.GetBytes(sent.body, "instructions").String())
		require.False(test, gjson.GetBytes(sent.body, "previous_response_id").Exists())
		require.Len(test, gjson.GetBytes(sent.body, "input").Array(), 1)
		require.Equal(test, session, gjson.GetBytes(sent.body, "client_metadata.session_id").String())
		require.Equal(test, thread, gjson.GetBytes(sent.body, "client_metadata.thread_id").String())
		if mode == "account" {
			require.NotEqual(test, session, cache)
		}
		metadata := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
		for _, field := range []string{"base_url", "openai_base_url"} {
			require.Equal(test, proxy.CodexBaseURL, metadata.Get(field).String())
			require.Equal(test, proxy.CodexBaseURL, gjson.GetBytes(sent.body, "client_metadata."+field).String())
			if useWS {
				// The persistent WS handshake keeps only its protocol allowlist;
				// per-request extension metadata travels in the response.create frame.
				require.False(test, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), field).Exists())
			} else {
				require.Equal(test, proxy.CodexBaseURL, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), field).String())
			}
		}
		require.Equal(test, thread+":0", metadata.Get("window_id").String())
		require.Equal(test, int64(0), metadata.Get("window_number").Int())
		require.Equal(test, "subagent", metadata.Get("thread_source").String())
		require.Equal(test, session, metadata.Get("session_id").String())
		require.Equal(test, thread, metadata.Get("thread_id").String())
		if useWS {
			require.Equal(test, "response.create", gjson.GetBytes(sent.body, "type").String())
			require.Equal(test, "permessage-deflate; client_max_window_bits", sent.headers.Get("Sec-WebSocket-Extensions"))
			require.Equal(test, thread, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), "thread_id").String())
		}
		require.Equal(test, account.AccountID, sent.headers.Get("Chatgpt-Account-Id"))
	}
}
