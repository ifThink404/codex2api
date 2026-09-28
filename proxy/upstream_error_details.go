package proxy

import (
	"encoding/json"
	"strings"

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

// upstreamErrorFields extracts only recognized error fields. In particular,
// validation error input/ctx fields may echo the entire prompt and must never
// enter diagnostics: a 422 detail array keeps only up to 8 loc/msg pairs.
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

type upstreamErrorFieldsDiagnostic struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Type    string `json:"type,omitempty"`
}

// upstreamClientErrorLogRecord is the file-log entry for other upstream 4xx
// (401/403/422/429...). It stores the extracted, redacted cause and a shape
// summary of the response, never the raw body that may echo request input.
func upstreamClientErrorLogRecord(body []byte) []byte {
	var detail *upstreamErrorFieldsDiagnostic
	if message, code, kind := upstreamErrorFields(body); message != "" {
		detail = &upstreamErrorFieldsDiagnostic{
			Message: security.SafeTruncate(security.MaskSensitiveData(message), 2048),
			Code:    security.SafeTruncate(security.MaskSensitiveData(code), 160),
			Type:    security.SafeTruncate(security.MaskSensitiveData(kind), 160),
		}
	}
	encoded, _ := json.Marshal(struct {
		Error    *upstreamErrorFieldsDiagnostic   `json:"error,omitempty"`
		Response *upstreamErrorResponseDiagnostic `json:"response"`
	}{detail, upstreamErrorResponseInfo(body, detail != nil)})
	return encoded
}
