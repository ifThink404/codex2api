package proxy

import (
	"net/http"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ApplyCodexAnalyticsMetadata(body []byte, headers http.Header) ([]byte, http.Header) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return body, headers
	}
	resolved := CodexRequestMetadataHeaders(headers, body)
	metadata := codexTurnMetadata(body, resolved)
	raw := "{}"
	if metadata.IsObject() {
		raw = metadata.Raw
	}
	for _, projection := range []struct {
		field  string
		flat   string
		header string
	}{
		{"session_id", "session_id", codexSessionIDHeader},
		{"thread_id", "thread_id", codexThreadIDHeader},
		{"window_id", "x-codex-window-id", codexWindowIDHeader},
		{"installation_id", "x-codex-installation-id", codexInstallationIDHeader},
		{"context_window_id", "x-codex-context-window-id", "X-Codex-Context-Window-Id"},
		{"parent_thread_id", "x-codex-parent-thread-id", codexParentThreadIDHeader},
		{"forked_from_thread_id", "x-codex-forked-from-thread-id", "X-Codex-Forked-From-Thread-Id"},
		{"subagent_kind", "subagent_kind", ""},
		{"turn_id", "turn_id", ""}, {"root_turn_id", "root_turn_id", ""},
		{"parent_turn_id", "parent_turn_id", ""}, {"thread_source", "thread_source", ""},
		{"request_kind", "request_kind", ""}, {"window_number", "window_number", ""},
		{"turn_started_at_unix_ms", "turn_started_at_unix_ms", ""},
	} {
		if gjson.Get(raw, projection.field).Exists() || codexTurnMetadataOmitsField(metadata, projection.field) {
			continue
		}
		value := gjson.GetBytes(body, "client_metadata."+projection.field)
		if !value.Exists() {
			value = gjson.GetBytes(body, "client_metadata."+projection.flat)
		}
		if value.Exists() {
			raw, _ = sjson.SetRaw(raw, projection.field, value.Raw)
		} else if header := resolved.Get(projection.header); header != "" {
			raw, _ = sjson.Set(raw, projection.field, header)
		} else if projection.field == "session_id" && resolved.Get(codexLegacySessionIDHeader) != "" {
			raw, _ = sjson.Set(raw, projection.field, resolved.Get(codexLegacySessionIDHeader))
		}
	}
	raw = omitCodexTurnMetadataIdentity(gjson.Parse(raw)).Raw
	enabled := codexTelemetryEnabled() && metadata.Get("analytics_enabled").Type != gjson.False
	raw, err := sjson.Set(raw, "analytics_enabled", enabled)
	if err != nil {
		return body, headers
	}
	const path = "client_metadata.x-codex-turn-metadata"
	var updated []byte
	if gjson.GetBytes(body, path).IsObject() {
		updated, err = sjson.SetRawBytes(body, path, []byte(raw))
	} else {
		updated, err = sjson.SetBytes(body, path, raw)
	}
	if err != nil {
		return body, headers
	}
	resolved.Set(codexTurnMetadataHeader, raw)
	return updated, resolved
}

func ApplyCodexAnalyticsHeader(headers http.Header, body []byte) {
	if metadata := codexTurnMetadata(body, nil); headers != nil && metadata.IsObject() {
		// Official compatibility headers omit the unbounded tool inventory.
		raw, _ := sjson.Delete(metadata.Raw, "tool_namespaces_info")
		headers.Set(codexTurnMetadataHeader, raw)
	}
}

func hasCodexAnalyticsState(metadata gjson.Result) bool {
	enabled := metadata.Get("analytics_enabled")
	return enabled.Type == gjson.True || enabled.Type == gjson.False
}
