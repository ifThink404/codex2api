package proxy

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These are two different official protocol enums, not spelling aliases.
func codexSubagentHeaderValue(kind string) string {
	if kind == "thread_spawn" {
		return "collab_spawn"
	}
	return kind
}

// Memory extraction omits thread identity from turn metadata. Compatibility
// headers/flat metadata still carry mapped identity for routing and continuity.
func codexTurnMetadataOmitsField(metadata gjson.Result, field string) bool {
	if metadata.Get("request_kind").String() != "memory" {
		if metadata.Get("thread_source").String() == "guardian_classifier" && !metadata.Get("request_kind").Exists() {
			switch field {
			case "installation_id", "window_id", "window_number", "context_window_id":
				return !metadata.Get(field).Exists()
			}
		}
		return false
	}
	switch field {
	case "installation_id", "session_id", "thread_id", "agent_name", "window_id", "window_number", "context_window_id":
		return true
	}
	return false
}

func omitCodexTurnMetadataIdentity(metadata gjson.Result) gjson.Result {
	for _, field := range []string{"installation_id", "session_id", "thread_id", "agent_name", "window_id", "window_number", "context_window_id"} {
		if codexTurnMetadataOmitsField(metadata, field) {
			raw, _ := sjson.Delete(metadata.Raw, field)
			metadata = gjson.Parse(raw)
		}
	}
	return metadata
}

// Current-frame controls win over handshake controls. Internal memory agents
// have a header marker but no nested subagent_kind; their ordinary turns are
// memgen requests. Extraction's request_kind=memory alone is not that signal.
func codexPassiveMarkers(headers http.Header, flat, canonical gjson.Result, snapshot bool) (subagent, memgen string) {
	flatValues := outboundMetadataAliases(outboundMetadataObject([]byte(flat.Raw)))
	kind := gjson.ParseBytes(outboundMetadataAliases(outboundMetadataObject([]byte(canonical.Raw)))["subagent_kind"])
	if !kind.Exists() {
		kind = gjson.ParseBytes(flatValues["subagent_kind"])
	}
	current := gjson.ParseBytes(flatValues["subagent_header"])
	if kind.Exists() {
		if kind.Type == gjson.String {
			subagent = codexSubagentHeaderValue(strings.TrimSpace(kind.String()))
		}
	} else if current.Exists() {
		if current.Type == gjson.String {
			subagent = strings.TrimSpace(current.String())
		}
	} else if !snapshot || canonical.IsObject() && canonical.Raw == headers.Get(codexTurnMetadataHeader) {
		subagent = headers.Get("X-OpenAI-Subagent")
	}
	flag := gjson.ParseBytes(flatValues["memgen_request"])
	if flag.Exists() {
		memgen = strings.ToLower(strings.TrimSpace(flag.String()))
	} else if subagent == "memory_consolidation" && !kind.Exists() {
		memgen = "true"
	} else if !snapshot || canonical.IsObject() && canonical.Raw == headers.Get(codexTurnMetadataHeader) {
		memgen = strings.ToLower(strings.TrimSpace(headers.Get("X-OpenAI-Memgen-Request")))
	}
	if memgen != "true" && memgen != "false" {
		memgen = ""
	}
	if len(subagent) > 1024 {
		subagent = ""
	}
	return subagent, memgen
}

func setCodexPassiveHeaders(headers http.Header, subagent, memgen string) {
	for name, value := range map[string]string{"X-OpenAI-Subagent": subagent, "X-OpenAI-Memgen-Request": memgen} {
		deleteHeaderCaseInsensitive(headers, name)
		if value != "" {
			headers.Set(name, value)
		}
	}
}
