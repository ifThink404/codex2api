package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
)

type upstreamErrorResponseDiagnostic struct {
	BodyBytes    int    `json:"body_bytes"`
	BodyFormat   string `json:"body_format"`
	MessageFound bool   `json:"message_found"`
}

func upstreamErrorResponseInfo(body []byte, messageFound bool) *upstreamErrorResponseDiagnostic {
	format := "non_json"
	if len(strings.TrimSpace(string(body))) == 0 {
		format = "empty"
	} else if gjson.ValidBytes(body) {
		format = "json"
	}
	return &upstreamErrorResponseDiagnostic{BodyBytes: len(body), BodyFormat: format, MessageFound: messageFound}
}

// Only recognized error fields are extracted. In particular, validation error
// input/ctx fields may echo the entire prompt and must never enter diagnostics.
func upstreamErrorFields(body []byte) (message, code, kind string) {
	parsed := gjson.ParseBytes(body)
	for _, path := range []string{"error", "response.error", "response.status_details.error", "detail", ""} {
		value := parsed
		if path != "" {
			value = parsed.Get(path)
		}
		if field := value.Get("message"); field.Type == gjson.String && strings.TrimSpace(field.String()) != "" {
			return field.String(), value.Get("code").String(), value.Get("type").String()
		}
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return value.String(), parsed.Get("code").String(), parsed.Get("type").String()
		}
	}
	if detail := parsed.Get("detail"); detail.IsArray() {
		var issues []string
		detail.ForEach(func(_, issue gjson.Result) bool {
			msg := issue.Get("msg")
			if msg.Type != gjson.String || strings.TrimSpace(msg.String()) == "" {
				return true
			}
			var location []any
			issue.Get("loc").ForEach(func(_, part gjson.Result) bool {
				if part.Type == gjson.String {
					location = append(location, security.SafeTruncate(part.String(), 80))
				} else if part.Type == gjson.Number {
					location = append(location, part.Int())
				}
				return len(location) < 12
			})
			prefix := ""
			if len(location) != 0 {
				encoded, _ := json.Marshal(location)
				prefix = string(encoded) + ": "
			}
			issues = append(issues, prefix+security.SafeTruncate(msg.String(), 500))
			return len(issues) < 8
		})
		if len(issues) > 0 {
			return strings.Join(issues, "; "), "", "validation_error"
		}
	}
	return "", "", ""
}

// Persist a small allowlist of numeric limits and reset delays, never cookies,
// authorization headers or arbitrary provider headers.
func upstreamRateLimitHeaders(headers http.Header) map[string]string {
	result := make(map[string]string)
	if value := normalizedRetryAfter(headers.Get("Retry-After")); value != "" {
		result["retry-after"] = value
	}
	for _, name := range []string{
		"x-ratelimit-limit-requests", "x-ratelimit-limit-tokens",
		"x-ratelimit-remaining-requests", "x-ratelimit-remaining-tokens",
		"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens",
		"x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset",
	} {
		value := strings.TrimSpace(headers.Get(name))
		if value == "" || len(value) > 64 {
			continue
		}
		_, numericErr := strconv.ParseUint(value, 10, 64)
		valid := numericErr == nil
		if strings.Contains(name, "reset") && !valid {
			delay, err := time.ParseDuration(value)
			valid = err == nil && delay >= 0
		}
		if valid {
			result[name] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
