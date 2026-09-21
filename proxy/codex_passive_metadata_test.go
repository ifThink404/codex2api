package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestPassiveMetadataCurrentSnapshotAndIdempotence(t *testing.T) {
	for _, encoded := range []bool{false, true} {
		for _, tc := range []struct{ name, canonical, flat, subagent, memgen string }{
			{"spawn", `{"request_kind":"turn","subagent_kind":"thread_spawn"}`, `{}`, "collab_spawn", ""},
			{"extraction", `{"request_kind":"memory","thread_source":"memory_consolidation"}`, `{}`, "", ""},
			{"internal-memory", `{"request_kind":"turn","thread_source":"memory_consolidation"}`, `{"x-openai-subagent":"memory_consolidation"}`, "memory_consolidation", "true"},
			{"ordinary-memory-subagent", `{"request_kind":"turn","subagent_kind":"memory_consolidation"}`, `{}`, "memory_consolidation", ""},
			{"clear", `{"request_kind":"turn","subagent_kind":null}`, `{}`, "", ""},
			{"explicit-false", `{"request_kind":"turn"}`, `{"x-openai-subagent":"memory_consolidation","x-openai-memgen-request":"false"}`, "memory_consolidation", "false"},
			{"custom-source", `{"request_kind":"turn","thread_source":"future_feature","subagent_kind":"custom_worker"}`, `{}`, "custom_worker", ""},
			{"flat-kind", `{"request_kind":"turn"}`, `{"subagent_kind":"thread_spawn","x-openai-subagent":"stale"}`, "collab_spawn", ""},
		} {
			t.Run(tc.name+map[bool]string{false: "/object", true: "/string"}[encoded], func(t *testing.T) {
				body := []byte(`{"client_metadata":` + tc.flat + `}`)
				if encoded {
					body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", tc.canonical)
				} else {
					body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(tc.canonical))
				}
				// Simulate an old connection's controls; the new frame is authoritative.
				headers := http.Header{"X-Openai-Subagent": {"old-worker"}, "X-Openai-Memgen-Request": {"true"}, "X-Codex-Turn-Metadata": {`{"request_kind":"old"}`}}
				for pass := 0; pass < 2; pass++ {
					body, headers = PrepareCodexOutboundMetadata(&auth.Account{DBID: 42}, body, headers)
					body, headers = ApplyCodexAnalyticsMetadata(body, headers)
					body, headers = FinalizeCodexOutboundMetadata(body, headers)
					require.Equal(t, tc.subagent, headers.Get("X-OpenAI-Subagent"))
					require.Equal(t, tc.memgen, headers.Get("X-OpenAI-Memgen-Request"))
					require.Equal(t, tc.subagent, gjson.GetBytes(body, "client_metadata.x-openai-subagent").String())
					require.Equal(t, tc.memgen, gjson.GetBytes(body, "client_metadata.x-openai-memgen-request").String())
					kind := gjson.Get(tc.canonical, "subagent_kind")
					if !kind.Exists() {
						kind = gjson.Get(tc.flat, "subagent_kind")
					}
					require.Equal(t, kind.String(), codexTurnMetadata(body, nil).Get("subagent_kind").String())
					require.NoError(t, ValidateCodexOutboundMetadata(body, headers))
				}
			})
		}
	}
}

func TestPassiveNativeWebsocketFrameDoesNotInheritMemgen(t *testing.T) {
	upgrade := http.Header{"X-Openai-Subagent": {"memory_consolidation"}, "X-Openai-Memgen-Request": {"true"}}
	main := []byte(`{"client_metadata":{"x-codex-turn-metadata":{"request_kind":"turn","thread_source":"user"}}}`)
	current := codexWebsocketCurrentFrameHeaders(upgrade, main)
	require.Empty(t, current.Get("X-OpenAI-Memgen-Request"))
	require.Empty(t, current.Get("X-OpenAI-Subagent"))
	internal := []byte(`{"client_metadata":{"x-openai-subagent":"memory_consolidation","x-codex-turn-metadata":{"request_kind":"turn","thread_source":"memory_consolidation"}}}`)
	current = codexWebsocketCurrentFrameHeaders(upgrade, internal)
	require.Equal(t, "true", current.Get("X-OpenAI-Memgen-Request"))
	require.Equal(t, "memory_consolidation", current.Get("X-OpenAI-Subagent"))
}

func TestMemoryRelayOmissionsStillMapFlatIdentity(t *testing.T) {
	account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.invalid", APIKey: "test-key"}
	body := []byte(`{"model":"gpt-6-astra","client_metadata":{"session_id":"private-root","thread_id":"private-root","x-codex-window-id":"private-root:1","x-codex-context-window-id":"private-context","x-codex-installation-id":"private-device","x-codex-turn-metadata":{"request_kind":"memory","thread_source":"memory_consolidation"}}}`)
	out, headers, err := prepareRelayOutboundPrivacy(context.Background(), account, body, nil)
	require.NoError(t, err)
	encoded, err := json.Marshal(headers)
	require.NoError(t, err)
	require.NotContains(t, string(out), "private-")
	require.NotContains(t, string(encoded), "private-")
	for _, field := range []string{"session_id", "thread_id", "window_id", "context_window_id", "installation_id"} {
		require.False(t, codexTurnMetadata(out, nil).Get(field).Exists(), field)
	}
	require.NotEmpty(t, headers.Get("Session-Id"))
	require.Equal(t, headers.Get("Session-Id"), headers.Get("Thread-Id"))
	require.Equal(t, headers.Get("Session-Id")+":1", headers.Get("X-Codex-Window-Id"))
	require.NoError(t, ValidateCodexOutboundMetadata(out, headers))
}

func TestPromptCachePurposeSeeds(t *testing.T) {
	body := []byte(`{"client_metadata":{"session_id":"root"}}`)
	keyFor := func(key, upstream string) string {
		request, _ := sjson.SetBytes(body, "prompt_cache_key", key)
		return ResolveCodexPromptCacheSeed(request, nil, upstream)
	}
	require.Equal(t, "scoped-root", keyFor("root", "scoped-root"))
	require.Equal(t, "scoped-root", keyFor("", "scoped-root"))
	require.Equal(t, "scoped-root", keyFor("scoped-root", "scoped-root"))
	review := keyFor("guardian:root", "scoped-root")
	require.NotEqual(t, "scoped-root", review)
	require.NotEqual(t, "guardian:root", review)
	require.Equal(t, review, keyFor("guardian:root", "scoped-root"))
	require.NotEqual(t, review, keyFor("guardian-v2:root", "scoped-root"))
	require.NotEqual(t, review, keyFor("guardian:root", "other-caller-root"))
	require.NotEqual(t, review, keyFor("custom-purpose", "scoped-root"))
	require.Equal(t, "explicit-stateless-key", keyFor("explicit-stateless-key", ""))
}
