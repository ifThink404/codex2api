package wsrelay

import (
	"context"
	"encoding/json"
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

// Official Codex source fixtures, checked at the actual HTTP/WS wire boundary.
func TestPassiveSourcesOfficialWire(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, scenario := range []struct{ name, source, kind, subHeader, subKind, memgen string }{
			{"subagent", "subagent", "turn", "collab_spawn", "thread_spawn", ""},
			{"memory_phase1", "memory_consolidation", "memory", "", "", ""},
			{"memory_phase2", "memory_consolidation", "turn", "memory_consolidation", "", "true"},
			{"system", "system", "turn", "", "", ""},
			{"reviewer", "guardian_review", "turn", "guardian", "", ""},
			{"classifier", "guardian_classifier", "", "guardian", "", ""},
		} {
			t.Run(transport+"/"+scenario.name, func(t *testing.T) {
				t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
				t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
				oldResin, oldRuntime := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings()
				t.Cleanup(func() { proxy.SetResinConfig(oldResin); proxy.ApplyRuntimeSettings(oldRuntime) })
				proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
				type capture struct {
					headers http.Header
					body    []byte
				}
				received := make(chan capture, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						b, _ := io.ReadAll(r.Body)
						received <- capture{r.Header.Clone(), b}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"audit_response","object":"response","status":"completed","output":[]}`)
						return
					}
					c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer c.Close()
					_, b, err := c.ReadMessage()
					if err != nil {
						return
					}
					received <- capture{r.Header.Clone(), b}
					_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"audit_response","status":"completed","output":[]}}`))
				}))
				t.Cleanup(server.Close)
				proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "passive-source-audit"})
				db, err := database.New("sqlite", filepath.Join(t.TempDir(), "audit.db"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 10*time.Second)
				defer cancel()
				account := &auth.Account{DBID: 42, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "audit-test-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice}
				const root = "01a09302-49f4-7b53-b545-91ef29610317"
				thread := root
				if scenario.name == "subagent" {
					thread = "01a09303-49f4-7b53-b545-920f29610317"
				}
				meta := map[string]any{"session_id": root, "thread_id": thread, "window_id": thread + ":0", "thread_source": scenario.source, "request_kind": scenario.kind, "turn_id": "01a0939f-d89c-77f1-94fa-080df9ebda48", "root_turn_id": "01a0939f-d89c-77f1-94fa-080df9ebda48"}
				if scenario.subKind != "" {
					meta["subagent_kind"] = scenario.subKind
					meta["parent_thread_id"] = root
				}
				if scenario.name == "memory_phase1" {
					delete(meta, "session_id")
					delete(meta, "thread_id")
					delete(meta, "window_id")
				}
				if scenario.name == "classifier" {
					delete(meta, "request_kind")
					delete(meta, "window_id")
					meta["guardian_classifier_source_thread_id"] = root
				}
				meta["deployment_ring"] = "canary"
				meta["externalRef"] = root
				raw, _ := json.Marshal(meta)
				cm := map[string]any{"session_id": root, "thread_id": thread, "x-codex-window-id": thread + ":0", "x-codex-turn-metadata": string(raw)}
				cm["ws_request_header_traceparent"] = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
				cm["ws_request_header_tracestate"] = "vendor=private-trace"
				cm["x-codex-ws-stream-request-start-ms"] = "1790000000123"
				cm["guardian_credits_requested"] = "true"
				if scenario.subHeader != "" {
					cm["x-openai-subagent"] = scenario.subHeader
				}
				headers := http.Header{"Session-Id": {root}, "Thread-Id": {thread}, "X-Codex-Turn-Metadata": {string(raw)}}
				if scenario.name == "reviewer" || scenario.name == "classifier" {
					headers.Set("X-Codex-Guardian", scenario.name)
				}
				if scenario.subHeader != "" {
					headers.Set("X-OpenAI-Subagent", scenario.subHeader)
				}
				if scenario.memgen != "" {
					headers.Set("X-OpenAI-Memgen-Request", scenario.memgen)
				}
				body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "client_metadata": cm, "prompt_cache_key": root})
				var response *http.Response
				if transport == "http" {
					response, err = proxy.ExecuteRequest(ctx, account, body, "cache-seed", "", "audit-key", nil, headers, false)
				} else {
					manager := NewManager()
					t.Cleanup(manager.Stop)
					result, e := NewExecutorWithManager(manager).ExecuteRequestViaWebsocket(ctx, account, body, "cache-seed", "", "audit-key", nil, headers, "")
					err = e
					if e == nil {
						response = websocketResponseToHTTP(ctx, result, http.StatusOK, nil)
					}
				}
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				sent := <-received
				sentMeta := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
				t.Logf("official sub_header=%q memgen_header=%q; sent sub_header=%q sub_flat=%q sub_nested=%q memgen_header=%q memgen_flat=%q source=%q kind=%q nested_thread_added=%v", scenario.subHeader, scenario.memgen, sent.headers.Get("X-OpenAI-Subagent"), gjson.GetBytes(sent.body, "client_metadata.x-openai-subagent").String(), sentMeta.Get("subagent_kind").String(), sent.headers.Get("X-OpenAI-Memgen-Request"), gjson.GetBytes(sent.body, "client_metadata.x-openai-memgen-request").String(), sentMeta.Get("thread_source").String(), sentMeta.Get("request_kind").String(), scenario.name == "memory_phase1" && sentMeta.Get("thread_id").Exists())
				require.Equal(t, scenario.source, sentMeta.Get("thread_source").String())
				require.Equal(t, scenario.kind, sentMeta.Get("request_kind").String())
				require.Equal(t, "canary", sentMeta.Get("deployment_ring").String())
				require.Equal(t, sent.headers.Get("Session-Id"), sentMeta.Get("externalRef").String())
				if scenario.name == "classifier" {
					require.Equal(t, sent.headers.Get("Session-Id"), sentMeta.Get("guardian_classifier_source_thread_id").String())
					require.False(t, sentMeta.Get("window_id").Exists())
					require.False(t, sentMeta.Get("installation_id").Exists())
				}
				if scenario.name == "reviewer" || scenario.name == "classifier" {
					require.Equal(t, scenario.name, sent.headers.Get("X-Codex-Guardian"))
				}
				require.Equal(t, scenario.name != "reviewer", gjson.GetBytes(sent.body, "client_metadata.guardian_credits_requested").Exists())
				trace := gjson.GetBytes(sent.body, "client_metadata.ws_request_header_traceparent").String()
				require.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`, trace)
				require.NotEqual(t, cm["ws_request_header_traceparent"], trace)
				require.NotContains(t, gjson.GetBytes(sent.body, "client_metadata.ws_request_header_tracestate").String(), "private-trace")
				require.Equal(t, "1790000000123", gjson.GetBytes(sent.body, "client_metadata.x-codex-ws-stream-request-start-ms").String())
				if transport == "http" {
					require.Equal(t, trace, sent.headers.Get("Traceparent"))
				} else {
					require.Empty(t, sent.headers.Get("Traceparent"))
					require.Empty(t, sent.headers.Get("Tracestate"))
				}

				require.Equal(t, scenario.subHeader, sent.headers.Get("X-OpenAI-Subagent"))
				require.Equal(t, scenario.subHeader, gjson.GetBytes(sent.body, "client_metadata.x-openai-subagent").String())
				require.Equal(t, scenario.subKind, sentMeta.Get("subagent_kind").String())
				require.Equal(t, scenario.memgen, sent.headers.Get("X-OpenAI-Memgen-Request"))
				require.Equal(t, scenario.memgen, gjson.GetBytes(sent.body, "client_metadata.x-openai-memgen-request").String())
				require.NoError(t, proxy.ValidateCodexOutboundMetadata(sent.body, sent.headers))
				require.NotEqual(t, root, sent.headers.Get("Session-Id"))
				require.NotEmpty(t, sent.headers.Get("Session-Id"))
				require.Equal(t, sent.headers.Get("Session-Id"), gjson.GetBytes(sent.body, "client_metadata.session_id").String())
				if scenario.name == "memory_phase1" {
					for _, field := range []string{"installation_id", "session_id", "thread_id", "agent_name", "window_id", "window_number", "context_window_id"} {
						require.False(t, sentMeta.Get(field).Exists(), field)
						require.False(t, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), field).Exists(), field)
					}
				}
			})
		}
	}
}

func TestExplicitCachePurposesAcrossTransportsRetriesAndAccounts(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	oldResin, oldRuntime := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.SetResinConfig(oldResin); proxy.ApplyRuntimeSettings(oldRuntime) })
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			body, _ := io.ReadAll(r.Body)
			received <- body
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"test-response","status":"completed","output":[]}`)
			return
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			received <- body
			_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"test-response","status":"completed","output":[]}}`))
		}
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "cache-purpose-test"})
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "cache.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 30*time.Second)
	defer cancel()
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	const root = "01a09302-49f4-7b53-b545-91ef29610317"
	const child = "01a09303-49f4-7b53-b545-920f29610317"
	keys := make(map[string]string)
	for _, account := range []*auth.Account{
		{DBID: 42, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "test-token-a"},
		{DBID: 43, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "test-token-b"},
	} {
		for _, transport := range []string{"http", "compact", "ws"} {
			for _, source := range []string{"user", "subagent", "guardian_review", "guardian_classifier", "custom_feature"} {
				thread, cache := child, root
				switch source {
				case "user":
					thread = root
				case "guardian_review":
					cache = "guardian:" + root
				case "guardian_classifier":
					cache = "guardian-v2:" + root
				case "custom_feature":
					cache = "custom:" + root
				}
				meta := map[string]any{"session_id": root, "thread_id": thread, "window_id": thread + ":0", "thread_source": source, "request_kind": "turn"}
				if thread != root {
					meta["parent_thread_id"] = root
				}
				metadata, _ := json.Marshal(meta)
				body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "prompt_cache_key": cache, "client_metadata": map[string]any{"session_id": root, "thread_id": thread, "x-codex-turn-metadata": string(metadata)}})
				headers := http.Header{"Session-Id": {root}, "Thread-Id": {thread}, "X-Codex-Turn-Metadata": {string(metadata)}}
				for pass := 0; pass < 2; pass++ {
					var response *http.Response
					switch transport {
					case "http":
						response, err = proxy.ExecuteRequest(ctx, account, body, "scoped-root", "", "test-key", nil, headers, false)
					case "compact":
						response, err = proxy.ExecuteCompactRequest(ctx, account, body, "scoped-root", "", "test-key", nil, headers)
					case "ws":
						var result *WsResponse
						result, err = executor.ExecuteRequestViaWebsocket(ctx, account, body, "scoped-root", "", "test-key", nil, headers, "")
						if err == nil {
							response = websocketResponseToHTTP(ctx, result, http.StatusOK, nil)
						}
					}
					require.NoError(t, err, "%s/%s/%d", transport, source, pass)
					_, err = io.Copy(io.Discard, response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					wire := <-received
					key := gjson.GetBytes(wire, "prompt_cache_key").String()
					require.NotEmpty(t, key)
					require.NotEqual(t, cache, key)
					require.NotContains(t, key, root)
					index := account.AccountID + "/" + source
					if previous := keys[index]; previous != "" {
						require.Equal(t, previous, key, "same purpose across transports and retries")
					} else {
						keys[index] = key
					}
				}
			}
		}
		prefix := account.AccountID + "/"
		require.Equal(t, keys[prefix+"user"], keys[prefix+"subagent"], "official root cache sharing")
		for _, source := range []string{"guardian_review", "guardian_classifier", "custom_feature"} {
			require.NotEqual(t, keys[prefix+"user"], keys[prefix+source])
		}
		require.NotEqual(t, keys[prefix+"guardian_review"], keys[prefix+"guardian_classifier"])
	}
	for _, source := range []string{"user", "subagent", "guardian_review", "guardian_classifier", "custom_feature"} {
		require.NotEqual(t, keys["661373c1-f1a9-4ca9-8682-a0594b30c36c/"+source], keys["761373c1-f1a9-4ca9-8682-a0594b30c36c/"+source], "account isolation")
	}
}
