package wsrelay

import (
	"net/http"

	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func prepareCodexHandshakeSnapshot(headers http.Header) {
	headers.Del("Traceparent")
	headers.Del("Tracestate")
	proxy.ClearCodexTurnStateHeaders(headers)
	// A pooled handshake is immutable. Per-turn/window/request identities must
	// live on the current frame, otherwise the next request has two identities.
	headers.Del("X-Codex-Window-Id")
	headers.Del("X-Codex-Context-Window-Id")
	if headers.Get("X-Client-Request-Id") != headers.Get("Thread-Id") {
		headers.Del("X-Client-Request-Id")
	}
	const name = "X-Codex-Turn-Metadata"
	raw := headers.Get(name)
	if raw == "" {
		return
	}
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		headers.Del(name)
		return
	}
	bounded := "{}"
	for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "installation_id", "thread_source", "subagent_kind", "analytics_enabled", "project_id", "workspace_id"} {
		value := gjson.Get(raw, field)
		if value.Exists() && !value.IsObject() && !value.IsArray() && len(value.Raw) <= 512 {
			bounded, _ = sjson.SetRaw(bounded, field, value.Raw)
		}
	}
	raw = bounded
	if len(raw) > 8192 {
		headers.Del(name)
	} else {
		headers.Set(name, raw)
	}
}

func stripCodexHandshakeSnapshotFromProfile(headers http.Header) {
	proxy.ClearCodexTurnStateHeaders(headers)
	metadata := gjson.Parse(headers.Get("X-Codex-Turn-Metadata"))
	if enabled := metadata.Get("analytics_enabled"); enabled.Type == gjson.True || enabled.Type == gjson.False {
		headers.Set("Codex-Profile-Analytics-Enabled", enabled.Raw)
	}
	for _, field := range []string{"session_id", "thread_id", "installation_id", "thread_source", "subagent_kind", "parent_thread_id", "forked_from_thread_id", "project_id", "workspace_id"} {
		if value := metadata.Get(field); value.Type == gjson.String && value.String() != "" {
			headers.Set("Codex-Profile-"+field, value.String())
		}
	}
	if kind := metadata.Get("request_kind").String(); kind != "" && kind != "turn" && kind != "compaction" {
		headers.Set("Codex-Profile-Request-Kind", kind)
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Codex-Window-Id", "X-Codex-Context-Window-Id"} {
		headers.Del(name)
	}
}
