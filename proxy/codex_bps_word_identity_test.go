package proxy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestWordBPSIdentityLifecycle(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "word.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := context.WithValue(WithCodexIdentityStore(t.Context(), db), bpsWordAccountScopeKey{}, "owner-account-a")
	headers := http.Header{"Session-Id": {"client-session"}, "X-Codex-Turn-Metadata": {`{"turn_id":"client-turn"}`}}
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"inspect"}],"context_management":[{"type":"compaction","compact_threshold":123456}]}`)
	project := func(ctx context.Context, b []byte, h http.Header, cache string) ([]byte, *bpsWordIdentityDiagnostic) {
		t.Helper()
		wire, d, err := prepareCodexBPSBodyForProfile(b, cache, false, false, h, bpsProfile(auth.BPSWord), ctx)
		require.NoError(t, err)
		require.True(t, d.WordIdentity.Persisted)
		for _, id := range []string{d.WordIdentity.TaskID, d.WordIdentity.TurnID} {
			u, err := uuid.Parse(id)
			require.NoError(t, err)
			require.EqualValues(t, 7, u.Version())
		}
		return wire, d.WordIdentity
	}
	wire, first := project(ctx, body, headers, "cache-window-0")
	require.Equal(t, "1", first.AgentIteration)
	require.NotEqual(t, first.TaskID, first.TurnID)
	require.EqualValues(t, 123456, gjson.GetBytes(wire, "context_management.0.compact_threshold").Int())
	require.False(t, gjson.GetBytes(wire, "prompt_cache_key").Exists())
	_, retry := project(ctx, body, headers, "cache-window-1")
	require.Equal(t, first.TaskID, retry.TaskID, "a cache/window rotation must not replace a task")
	require.Equal(t, first.TurnID, retry.TurnID)
	require.True(t, retry.ReusedStep)
	input := `[{"role":"user","content":"inspect"},{"type":"function_call","name":"a","call_id":"a","arguments":"{}"},{"type":"function_call","name":"b","call_id":"b","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":{"n":9007199254740993}},{"type":"function_call_output","call_id":"b","output":"ok"}]`
	tools, err := sjson.SetRawBytes(body, "input", []byte(input))
	require.NoError(t, err)
	_, second := project(ctx, tools, headers, "cache-window-1")
	require.Equal(t, "2", second.AgentIteration)
	require.Equal(t, first.TurnID, second.TurnID)
	items := gjson.GetBytes(tools, "input").Array()
	items[3], items[4] = items[4], items[3]
	reordered := `[` + items[0].Raw + `,` + items[1].Raw + `,` + items[2].Raw + `,` + items[3].Raw + `,` + items[4].Raw + `]`
	reorderedBody, err := sjson.SetRawBytes(tools, "input", []byte(reordered))
	require.NoError(t, err)
	_, repeat := project(ctx, reorderedBody, headers, "cache-window-2")
	require.Equal(t, "2", repeat.AgentIteration)
	require.True(t, repeat.ReusedStep)
	tools, err = sjson.SetRawBytes(tools, "input.-1", []byte(`{"type":"function_call","name":"c","call_id":"c","arguments":"{}"}`))
	require.NoError(t, err)
	tools, err = sjson.SetRawBytes(tools, "input.-1", []byte(`{"type":"function_call_output","call_id":"c","output":"done"}`))
	require.NoError(t, err)
	_, third := project(ctx, tools, headers, "cache-window-2")
	require.Equal(t, "3", third.AgentIteration)
	changedHeaders := headers.Clone()
	changedHeaders.Set("X-Codex-Turn-Metadata", `{"turn_id":"next-user-turn"}`)
	_, next := project(ctx, body, changedHeaders, "cache-window-2")
	require.Equal(t, first.TaskID, next.TaskID)
	require.NotEqual(t, first.TurnID, next.TurnID)
	require.Equal(t, "1", next.AgentIteration)
	switched := context.WithValue(ctx, bpsWordAccountScopeKey{}, "owner-account-b")
	_, switchedIdentity := project(switched, tools, headers, "cache-window-2")
	require.NotEqual(t, first.TaskID, switchedIdentity.TaskID)
	require.NotEqual(t, first.TurnID, switchedIdentity.TurnID)
	require.Equal(t, "1", switchedIdentity.AgentIteration)
	rebound := context.WithValue(ctx, sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{key: "root", record: database.SessionContinuityRecord{OutboundWindowReset: true, FailoverCount: 2}})
	_, reboundIdentity := project(rebound, tools, headers, "cache-window-2")
	require.NotEqual(t, first.TaskID, reboundIdentity.TaskID)
	require.NotEqual(t, first.TurnID, reboundIdentity.TurnID)
	require.Equal(t, "1", reboundIdentity.AgentIteration)
	require.EqualValues(t, 2, reboundIdentity.Generation)
	_, reboundRetry := project(rebound, tools, headers, "cache-window-2")
	require.Equal(t, "1", reboundRetry.AgentIteration)
	require.True(t, reboundRetry.ReusedStep)
	newSession := headers.Clone()
	newSession.Set("Session-Id", "other-session")
	_, other := project(ctx, body, newSession, "cache-window-2")
	require.NotEqual(t, first.TaskID, other.TaskID)
	_, inferredFirst := project(ctx, body, http.Header{}, "inferred-conversation")
	_, inferredTools := project(ctx, tools, http.Header{}, "inferred-conversation")
	require.Equal(t, inferredFirst.TurnID, inferredTools.TurnID)
	require.Equal(t, "2", inferredTools.AgentIteration)
	nextUser, err := sjson.SetRawBytes(tools, "input.-1", []byte(`{"role":"user","content":"inspect"}`))
	require.NoError(t, err)
	_, inferredNext := project(ctx, nextUser, http.Header{}, "inferred-conversation")
	require.Equal(t, inferredFirst.TaskID, inferredNext.TaskID)
	require.NotEqual(t, inferredFirst.TurnID, inferredNext.TurnID)
	require.Equal(t, "1", inferredNext.AgentIteration)
}

func TestWordBPSHeadersAndDiagnostics(t *testing.T) {
	old := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(old) })
	settings := old
	settings.CodexUserAgentConfig = `{"bps_word_user_agent":"WordTest/1"}`
	ApplyRuntimeSettings(settings)
	h := http.Header{"Version": {"old"}, "Originator": {"old"}, "X-Openai-Internal-Basispoints-Office-Host-Version": {"16.113"}}
	applyCodexBPSHeadersForProfile(h, &auth.Account{AccountID: "account"}, "secret", "cache", false, bpsProfile(auth.BPSWord))
	require.Equal(t, "WordTest/1", h.Get("User-Agent"))
	require.Equal(t, "OfficeOnline", h.Get("X-Openai-Internal-Basispoints-Office-Platform"))
	for _, name := range []string{"Version", "Originator", "Session-Id", "X-Openai-Internal-Basispoints-Office-Host-Version", "X-Openai-Internal-Basispoints-Tools-Version-Id", "X-Openai-Internal-Basispoints-Client-Device-Id", "X-Openai-Internal-Codex-Responses-Lite"} {
		require.Empty(t, h.Get(name))
	}
	require.Equal(t, "Bearer secret", h.Get("Authorization"))
	_, _, nativeOverride := codexUserAgentFromConfig(settings.CodexUserAgentConfig, 1, "")
	require.False(t, nativeOverride)
	normalized, err := NormalizeCodexUserAgentConfigJSON(settings.CodexUserAgentConfig)
	require.NoError(t, err)
	require.Contains(t, normalized, "WordTest/1")
	_, err = NormalizeCodexUserAgentConfigJSON(`{"bps_word_user_agent":"unsafe\r\nInjected: yes"}`)
	require.Error(t, err)
	body := []byte(`{"model":"gpt-6-astra","input":"hi"}`)
	wire, d, err := prepareCodexBPSBody(body, "cache", false)
	require.NoError(t, err)
	captured := captureOutboundIdentityBody(wire)
	require.Equal(t, d.WordIdentity.TaskID, captured.wire["metadata"].(map[string]string)["task_id"])
	require.Equal(t, d.WordIdentity.TurnID, captured.wire["metadata"].(map[string]string)["turn_id"])
	require.False(t, gjson.GetBytes(wire, "context_management").Exists())
	settings.CodexUserAgentConfig = "{}"
	ApplyRuntimeSettings(settings)
	require.Equal(t, defaultBPSWordUserAgent, bpsWordUserAgent())
}
