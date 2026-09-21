package admin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func assertCodexIndependentTestIdentity(test *testing.T, body []byte, outbound ...bool) string {
	test.Helper()
	metadata := gjson.GetBytes(body, "client_metadata")
	turnMetadata := gjson.Parse(metadata.Get("x-codex-turn-metadata").String())
	sessionID := metadata.Get("session_id").String()
	threadID := metadata.Get("thread_id").String()
	turnID := metadata.Get("turn_id").String()
	parentTurnID := metadata.Get("parent_turn_id").String()
	for _, identifier := range []string{sessionID, threadID, turnID, parentTurnID, turnMetadata.Get("context_window_id").String()} {
		if _, err := uuid.Parse(identifier); err != nil {
			test.Fatalf("invalid test identifier %q: %v", identifier, err)
		}
	}
	if sessionID == threadID || sessionID == turnID || metadata.Get("x-codex-window-id").String() != threadID+":0" {
		test.Fatalf("inconsistent independent identity: %s", metadata.Raw)
	}
	for field, expected := range map[string]string{
		"session_id": sessionID, "thread_id": threadID, "turn_id": turnID,
		"window_id": threadID + ":0", "request_kind": "turn",
		"parent_thread_id": sessionID, "parent_turn_id": parentTurnID, "root_turn_id": parentTurnID,
		"thread_source": "subagent", "subagent_kind": "thread_spawn", "window_number": "0",
	} {
		if actual := turnMetadata.Get(field).String(); actual != expected {
			test.Errorf("turn metadata %s = %q, want %q", field, actual, expected)
		}
	}
	for _, field := range []string{"root_session_id", "root_fingerprint", "forked_from_thread_id", "passive_feature"} {
		if metadata.Get(field).Exists() || turnMetadata.Get(field).Exists() {
			test.Errorf("unexpected inherited or invented field %q", field)
		}
	}
	flatSubagent := "thread_spawn" // The local test payload precedes transport normalization.
	if len(outbound) > 0 && outbound[0] {
		flatSubagent = "collab_spawn"
	}
	require.Equal(test, flatSubagent, metadata.Get("x-openai-subagent").String())
	require.False(test, metadata.Get("x-openai-memgen-request").Exists())
	require.Equal(test, "medium", gjson.GetBytes(body, "reasoning.effort").String())
	require.True(test, gjson.GetBytes(body, "instructions").Exists())
	require.Empty(test, gjson.GetBytes(body, "instructions").String())
	started := turnMetadata.Get("turn_started_at_unix_ms").Int()
	if started <= 0 || time.Since(time.UnixMilli(started)).Abs() > time.Minute {
		test.Fatalf("test start time = %d", started)
	}
	if gjson.GetBytes(body, "prompt_cache_key").String() != sessionID || proxy.ResolveExplicitSessionID(nil, body) != sessionID {
		test.Fatal("test transport identity differs from the independent session")
	}
	headers := proxy.CodexRequestMetadataHeaders(nil, body)
	for name, expected := range map[string]string{
		"Session-Id": sessionID, "Thread-Id": threadID,
		"X-Codex-Window-Id": threadID + ":0", "X-Client-Request-Id": threadID,
		"X-Codex-Parent-Thread-Id": sessionID, "X-OpenAI-Subagent": "collab_spawn",
	} {
		if actual := headers.Get(name); actual != expected {
			test.Errorf("projected %s = %q, want %q", name, actual, expected)
		}
	}
	return sessionID
}

func TestCodexIndependentTestPayloadKeepsRootWithFreshThreadsAndDevice(test *testing.T) {
	account := &auth.Account{DBID: 42, CodexInstallationID: "11111111-aaaa-4111-8111-111111111111"}
	session, err := proxy.ResolveCodexAccountTestSessionID(context.Background(), nil, account)
	require.NoError(test, err)
	seen := make(map[string]bool)
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		account.CodexFingerprintMode = mode
		payload := buildCodexIndependentTestPayload(account, "gpt-5.5", "Reply OK", session)
		sessionID := assertCodexIndependentTestIdentity(test, payload)
		require.Equal(test, session, sessionID)
		for _, field := range []string{"thread_id", "turn_id", "parent_turn_id"} {
			value := gjson.GetBytes(payload, "client_metadata."+field).String()
			require.False(test, seen[value], "tests must create fresh %s", field)
			seen[value] = true
		}
		metadata := gjson.GetBytes(payload, "client_metadata")
		canonical := gjson.Parse(metadata.Get("x-codex-turn-metadata").String())
		if metadata.Get("x-codex-installation-id").String() != account.CodexInstallationID || canonical.Get("installation_id").String() != account.CodexInstallationID {
			test.Fatal("test changed the persisted installation ID")
		}
		if gjson.GetBytes(payload, "input.0.content.0.text").String() != "Reply OK" || gjson.GetBytes(payload, "model").String() != "gpt-5.5" {
			test.Fatal("test changed the requested content or model")
		}
	}
	account.CustomHeaders = map[string]string{"X-Codex-Installation-Id": "22222222-bbbb-4222-8222-222222222222"}
	payload := buildCodexIndependentTestPayload(account, "gpt-5.5", "Reply OK", session)
	if gjson.GetBytes(payload, "client_metadata.x-codex-installation-id").String() != account.CustomHeaders["X-Codex-Installation-Id"] {
		test.Fatal("custom installation ID was not respected")
	}
}

func TestCodexIndependentTestPayloadDoesNotAffectOtherProviders(test *testing.T) {
	handler := &Handler{}
	accounts := []*auth.Account{
		{DBID: 41},
		{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.com", APIKey: "test-key"},
		{DBID: 43, UpstreamType: auth.UpstreamGrok, APIKey: "test-key"},
	}
	for index, account := range accounts {
		payload, err := handler.buildAccountConnectionTestPayload(context.Background(), account, "gpt-5.5", auth.ClaudeSecurityConfig{})
		require.NoError(test, err)
		if index == 0 {
			assertCodexIndependentTestIdentity(test, payload)
		} else if gjson.GetBytes(payload, "client_metadata").Exists() || gjson.GetBytes(payload, "prompt_cache_key").Exists() {
			test.Fatalf("provider %q received Codex-only test metadata", account.UpstreamType)
		}
	}
}

func TestCodexIndependentSingleAndBatchTestsKeepSelectedAccount(test *testing.T) {
	previousSettings := proxy.CurrentRuntimeSettings()
	settings := previousSettings
	settings.CodexForceWebsocket = true
	proxy.ApplyRuntimeSettings(settings)
	test.Cleanup(func() { proxy.ApplyRuntimeSettings(previousSettings) })
	previousExecutor := proxy.WebsocketExecuteFunc
	test.Cleanup(func() { proxy.WebsocketExecuteFunc = previousExecutor })
	store := auth.NewStore(nil, nil, &database.SystemSettings{TestModel: "gpt-5.5"})
	test.Cleanup(store.Stop)
	account := &auth.Account{DBID: 42, AccessToken: "test-token", AccountID: "selected-account", Status: auth.StatusReady}
	store.AddAccount(account)
	handler := &Handler{store: store}
	sessions := make(chan string, 4)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, selected *auth.Account, body []byte, sessionID, proxyURL, apiKey string, deviceCfg *proxy.DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		if selected != account {
			test.Errorf("test switched the administrator-selected account")
		}
		generatedSession := assertCodexIndependentTestIdentity(test, body, true)
		if sessionID != generatedSession || poolRouteKey != "" {
			test.Errorf("test used another or shared transport session: %q / %q", sessionID, poolRouteKey)
		}
		require.Equal(test, "collab_spawn", headers.Get("X-OpenAI-Subagent"))
		require.Equal(test, sessionID, headers.Get("X-Codex-Parent-Thread-Id"))
		require.Empty(test, headers.Get("X-OpenAI-Memgen-Request"))
		sessions <- generatedSession
		stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}
	recorder := serveCodexDiagnosticsTest(handler)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"success":true`) {
		test.Fatalf("single test failed: %d %s", recorder.Code, recorder.Body.String())
	}
	status, message := handler.runSingleBatchTest(context.Background(), account)
	if status != "success" {
		test.Fatalf("batch test failed: %s %s", status, message)
	}
	if len(sessions) != 2 {
		test.Fatalf("test calls = %d, want 2", len(sessions))
	}
	if first, second := <-sessions, <-sessions; first != second {
		test.Fatal("single and batch tests changed the fixed account root")
	}
}
