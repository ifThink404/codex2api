// Package upstreamprivacy removes private routing addresses at public response
// boundaries. Connection targets and local diagnostic records are unchanged.
package upstreamprivacy

import (
	"fmt"
	"regexp"
	"strings"
)

const host = "bps.openai.com"
const replacement = "chatgpt.com"

type literalRule struct {
	forms       [][]string
	parts       []*regexp.Regexp
	pattern     *regexp.Regexp
	replacement string
}

func newLiteralRule(source, replacement string) literalRule {
	result := make([][]string, len(source))
	for i := range source {
		b := source[i]
		result[i] = []string{string(b), fmt.Sprintf("%%%02x", b), fmt.Sprintf("%%25%02x", b), fmt.Sprintf("%%2525%02x", b)}
		for slashes := 1; slashes <= 8; slashes++ {
			result[i] = append(result[i], strings.Repeat(`\`, slashes)+fmt.Sprintf("u%04x", b))
			if b == '/' {
				result[i] = append(result[i], strings.Repeat(`\`, slashes)+"/")
			}
		}
	}
	parts := make([]*regexp.Regexp, len(result))
	for i, variants := range result {
		quoted := make([]string, len(variants))
		for j, value := range variants {
			quoted[j] = regexp.QuoteMeta(value)
		}
		parts[i] = regexp.MustCompile("(?i)(?:" + strings.Join(quoted, "|") + ")")
	}
	var pattern strings.Builder
	for _, part := range parts {
		pattern.WriteString(part.String())
	}
	return literalRule{result, parts, regexp.MustCompile(pattern.String()), replacement}
}

var addressRules = []literalRule{
	newLiteralRule("hidden.invalid/basispoints/api", "chatgpt.com/backend-api/codex"),
	newLiteralRule(host, replacement),
	newLiteralRule("/basispoints/api", "/backend-api/codex"),
}

// Source markers are only removed from provider-generated text, not caller
// tool arguments, schemas, opaque history, or authentication material.
var sourceRules = []literalRule{
	newLiteralRule("Basis Points", "AI Assistant"),
	newLiteralRule("basispoints", "upstreamapi"),
	newLiteralRule("bps_", "api_"),
}

// Preserve escape representation so JSON and embedded tool arguments remain
// valid. Stream callers must repartition replacements that change byte length.
func Text(value string) string {
	for _, rule := range addressRules {
		value = rule.text(value)
	}
	return value
}

func SourceText(value string) string {
	value = Text(value)
	for _, rule := range sourceRules {
		value = rule.text(value)
	}
	return value
}

func (rule literalRule) text(value string) string {
	return rule.pattern.ReplaceAllStringFunc(value, func(match string) string {
		var result strings.Builder
		tokens := make([]string, 0, len(rule.parts))
		for _, part := range rule.parts {
			index := part.FindStringIndex(match)
			token := match[:index[1]]
			tokens = append(tokens, token)
			match = match[index[1]:]
		}
		for i := range rule.replacement {
			token := tokens[min(i, len(tokens)-1)]
			switch {
			case token[0] == '%':
				result.WriteString(token[:len(token)-2] + fmt.Sprintf("%02x", rule.replacement[i]))
			case token[0] == '\\':
				if token[len(token)-1] == '/' {
					if rule.replacement[i] == '/' {
						result.WriteString(token)
					} else {
						result.WriteString(token[:len(token)-1] + fmt.Sprintf("u%04x", rule.replacement[i]))
					}
				} else {
					result.WriteString(token[:len(token)-4] + fmt.Sprintf("%04x", rule.replacement[i]))
				}
			default:
				result.WriteByte(rule.replacement[i])
			}
		}
		return result.String()
	})
}

func Bytes(value []byte) []byte { return []byte(Text(string(value))) }

// Prefix identifies an unfinished private address, including mixed escaping.
// The longest supported literal needs fewer than 400 bytes of lookbehind.
func Prefix(value string) bool {
	for _, rule := range addressRules {
		if rule.prefix(value) {
			return true
		}
	}
	return false
}

func SourcePrefix(value string) bool {
	if Prefix(value) {
		return true
	}
	for _, rule := range sourceRules {
		if rule.prefix(value) {
			return true
		}
	}
	return false
}

func (rule literalRule) prefix(value string) bool {
	value = strings.ToLower(value)
	for _, variants := range rule.forms {
		if value == "" {
			return true
		}
		matched := false
		for _, original := range variants {
			form := strings.ToLower(original)
			if len(value) < len(form) && strings.HasPrefix(form, value) {
				return true
			}
			if strings.HasPrefix(value, form) {
				value = value[len(form):]
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return false
}
