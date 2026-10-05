package proxy

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fork-owned since upstream v3.0.6 deleted proxy/basispoints: the BPS tool
// relay hands spawn_agent messages back as plaintext, and clients wrap them
// in an encrypted schema field. Replayed history must carry them as text, on
// BPS and on the same account's native fallback alike.

// isPlaintextBPSAgentContent recognizes text produced by the BPS tool relay
// that the client wrapped in an encrypted schema field. Opaque tokens, wrapped
// base64 and native Fernet context are deliberately left untouched.
func isPlaintextBPSAgentContent(value string) bool {
	if !utf8.ValidString(value) || strings.HasPrefix(strings.TrimSpace(value), "gAAAA") {
		return false
	}
	hasLetter, hasSpace, hasNonASCIILetter := false, false, false
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
		if unicode.IsLetter(r) {
			hasLetter = true
			if r > unicode.MaxASCII {
				hasNonASCIILetter = true
			}
		}
		if r == ' ' {
			hasSpace = true
		}
	}
	return hasNonASCIILetter || (hasLetter && hasSpace && len(strings.Fields(value)) > 1)
}

// normalizeBPSAgentMessage repairs only clearly plaintext encrypted parts. It
// preserves the complete text, author, recipient and real encrypted context.
func normalizeBPSAgentMessage(item map[string]any) bool {
	if itemType, _ := item["type"].(string); itemType != "agent_message" {
		return false
	}
	content, _ := item["content"].([]any)
	changed := false
	for _, raw := range content {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if partType, _ := part["type"].(string); partType != "encrypted_content" {
			continue
		}
		value, ok := part["encrypted_content"].(string)
		if !ok || !isPlaintextBPSAgentContent(value) {
			continue
		}
		if _, conflict := part["text"]; conflict {
			continue
		}
		part["type"] = "input_text"
		part["text"] = value
		delete(part, "encrypted_content")
		changed = true
	}
	return changed
}
