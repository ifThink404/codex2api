package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const defaultCodexReasoningEffort = "low"

// Apply at the Codex execution boundary, after configured payload rules. Relay
// passthrough and other providers must retain their own request semantics.
func defaultCodexReasoning(body []byte) []byte {
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		value := gjson.GetBytes(body, path)
		if value.Exists() && value.Type != gjson.Null && (value.Type != gjson.String || strings.TrimSpace(value.String()) != "") {
			return body
		}
	}
	if reasoning := gjson.GetBytes(body, "reasoning"); reasoning.Exists() && reasoning.Type != gjson.Null && !reasoning.IsObject() {
		return body
	}
	updated, err := sjson.SetBytes(body, "reasoning.effort", defaultCodexReasoningEffort)
	if err != nil {
		return body
	}
	return updated
}

func diagnosticReasoningEffort(body []byte) string {
	switch effort := strings.ToLower(strings.TrimSpace(extractReasoningEffort(body))); effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return effort
	default:
		return ""
	}
}
