// Package upstreamprivacy removes private routing addresses at public response
// boundaries. Connection targets and local diagnostic records are unchanged.
package upstreamprivacy

import (
	"fmt"
	"regexp"
	"strings"
)

const host = "bps.openai.com"
const replacement = "hidden.invalid" // same byte length, including escaped forms

var forms = func() [][]string {
	result := make([][]string, len(host))
	for i := range host {
		b := host[i]
		result[i] = []string{string(b), fmt.Sprintf("%%%02x", b), fmt.Sprintf("%%25%02x", b), fmt.Sprintf("%%2525%02x", b)}
		for slashes := 1; slashes <= 8; slashes++ {
			result[i] = append(result[i], strings.Repeat(`\`, slashes)+fmt.Sprintf("u%04x", b))
		}
	}
	return result
}()

var parts = func() []*regexp.Regexp {
	result := make([]*regexp.Regexp, len(forms))
	for i, variants := range forms {
		quoted := make([]string, len(variants))
		for j, value := range variants {
			quoted[j] = regexp.QuoteMeta(value)
		}
		result[i] = regexp.MustCompile("(?i)(?:" + strings.Join(quoted, "|") + ")")
	}
	return result
}()

var address = func() *regexp.Regexp {
	var pattern strings.Builder
	for _, part := range parts {
		pattern.WriteString(part.String())
	}
	return regexp.MustCompile(pattern.String())
}()

// Preserve the representation and byte length of every character so JSON
// escapes and streamed tool arguments remain syntactically valid.
func Text(value string) string {
	return address.ReplaceAllStringFunc(value, func(match string) string {
		var result strings.Builder
		for i, part := range parts {
			index := part.FindStringIndex(match)
			token := match[:index[1]]
			switch {
			case token[0] == '%':
				result.WriteString(token[:len(token)-2] + fmt.Sprintf("%02x", replacement[i]))
			case token[0] == '\\':
				result.WriteString(token[:len(token)-4] + fmt.Sprintf("%04x", replacement[i]))
			default:
				result.WriteByte(replacement[i])
			}
			match = match[index[1]:]
		}
		return result.String()
	})
}

func Bytes(value []byte) []byte { return []byte(Text(string(value))) }

// Prefix identifies an unfinished protected hostname, including mixed URL and
// JSON escaping. At most 182 bytes need retaining between stream events.
func Prefix(value string) bool {
	value = strings.ToLower(value)
	for _, variants := range forms {
		if value == "" {
			return true
		}
		matched := false
		for _, form := range variants {
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
