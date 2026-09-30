package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWordBPSHeadersAndDiagnostics(t *testing.T) {
	previous := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previous) })
	storeBPSConfig(BPSConfig{WordUserAgent: "WordTest/1"})
	h := http.Header{"Version": {"old"}, "Originator": {"old"}, "X-Openai-Internal-Basispoints-Office-Host-Version": {"16.113"}}
	applyCodexBPSHeadersForProfile(h, &auth.Account{AccountID: "account"}, "secret", "cache", false, bpsProfile(auth.BPSWord))
	require.Equal(t, "WordTest/1", h.Get("User-Agent"))
	require.Equal(t, "OfficeOnline", h.Get("X-Openai-Internal-Basispoints-Office-Platform"))
	for _, name := range []string{"Version", "Originator", "Session-Id", "X-Openai-Internal-Basispoints-Office-Host-Version", "X-Openai-Internal-Basispoints-Tools-Version-Id", "X-Openai-Internal-Basispoints-Client-Device-Id", "X-Openai-Internal-Codex-Responses-Lite"} {
		require.Empty(t, h.Get(name))
	}
	require.Equal(t, "Bearer secret", h.Get("Authorization"))
	// The Word UA is plugin config: it can never enable a native UA override.
	_, _, nativeOverride := codexUserAgentFromConfig(CurrentRuntimeSettings().CodexUserAgentConfig, 1, "")
	require.False(t, nativeOverride)
	require.NoError(t, bpsPlugin{}.ValidateConfig([]byte(`{"word_user_agent":"WordTest/1"}`)))
	require.Error(t, bpsPlugin{}.ValidateConfig([]byte(`{"word_user_agent":"unsafe\r\nInjected: yes"}`)))
	body := []byte(`{"model":"gpt-6-astra","input":"hi"}`)
	wire, d, err := prepareCodexBPSBody(body, "cache", false)
	require.NoError(t, err)
	require.Equal(t, d.WordIdentity.TaskID, gjson.GetBytes(wire, "metadata.task_id").String())
	require.Equal(t, d.WordIdentity.TurnID, gjson.GetBytes(wire, "metadata.turn_id").String())
	require.False(t, gjson.GetBytes(wire, "context_management").Exists())
	storeBPSConfig(BPSConfig{})
	require.Equal(t, defaultBPSWordUserAgent, bpsWordUserAgent())
}
