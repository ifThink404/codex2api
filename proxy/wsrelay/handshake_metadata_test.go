package wsrelay

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexHandshakeSnapshotKeepsIdentityAndBoundsMetadata(test *testing.T) {
	headers := http.Header{}
	headers.Set("Session-Id", "root")
	headers.Set("Thread-Id", "child")
	headers.Set("X-Client-Request-Id", "child")
	headers.Set("X-Codex-Parent-Thread-Id", "parent")
	headers.Set("X-OpenAI-Memgen-Request", "true")
	headers.Set("X-Codex-Turn-State", "private-token")
	headers.Set("X-Codex-Window-Id", "child:2")
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"root","thread_id":"child","window_number":2,"tool_namespaces_info":"`+strings.Repeat("tool", 10000)+`","other":"`+strings.Repeat("large", 3000)+`"}`)
	prepareCodexHandshakeSnapshot(headers)
	require.Equal(test, "root", headers.Get("Session-Id"))
	require.Equal(test, "child", headers.Get("Thread-Id"))
	require.Equal(test, "child", headers.Get("X-Client-Request-Id"))
	require.Equal(test, "parent", headers.Get("X-Codex-Parent-Thread-Id"))
	require.Equal(test, "true", headers.Get("X-OpenAI-Memgen-Request"))
	require.Empty(test, headers.Get("X-Codex-Turn-State"))
	require.LessOrEqual(test, len(headers.Get("X-Codex-Turn-Metadata")), 8192)
	require.False(test, gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "tool_namespaces_info").Exists())
	require.False(test, gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "window_number").Exists())
	require.Empty(test, headers.Get("X-Codex-Window-Id"))
	profile := websocketConnectionProfile(headers)
	for _, field := range []string{"Session-Id", "Thread-Id", "X-Client-Request-Id", "X-Codex-Parent-Thread-Id", "X-Codex-Forked-From-Thread-Id", "Chatgpt-Account-Id", "X-OpenAI-Subagent", "X-OpenAI-Memgen-Request", "X-Codex-Installation-Id"} {
		changed := headers.Clone()
		changed.Set(field, "different")
		require.NotEqual(test, profile, websocketConnectionProfile(changed), field)
	}
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"root","thread_id":"child","window_number":3,"turn_id":"new","request_kind":"compaction"}`)
	headers.Set("X-Codex-Window-Id", "child:3")
	headers.Set("X-Codex-Turn-State", "next-token")
	require.Equal(test, profile, websocketConnectionProfile(headers))
}

func TestProjectIdentityHandshakeProfile(t *testing.T) {
	headers := http.Header{"X-Codex-Turn-Metadata": []string{`{"project_id":"mapped-project","workspace_id":"mapped-workspace"}`}}
	prepareCodexHandshakeSnapshot(headers)
	require.Equal(t, "mapped-project", gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "project_id").String())
	profile := websocketConnectionProfile(headers)
	for _, field := range []string{"project_id", "workspace_id"} {
		other := headers.Clone()
		raw, _ := sjson.Set(other.Get("X-Codex-Turn-Metadata"), field, "another")
		other.Set("X-Codex-Turn-Metadata", raw)
		require.NotEqual(t, profile, websocketConnectionProfile(other))
	}
	// Stateless pooled connections carry request-specific projects on frames.
	headers.Set("X-Codex-Project-Id", "mapped-project")
	headers.Set("X-Codex-Workspace-Id", "mapped-workspace")
	stripCodexFrameScopedHandshakeHeaders(headers)
	require.Empty(t, headers.Get("X-Codex-Project-Id"))
	require.Empty(t, headers.Get("X-Codex-Workspace-Id"))
}
