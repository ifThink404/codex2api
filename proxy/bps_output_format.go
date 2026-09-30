package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tidwall/gjson"
)

// Structured output (adopted from upstream's official Excel BPS adapter). The
// BPS wire body has no structured-output field, so Responses text.format
// becomes a developer instruction. BPS does not enforce it: the model is only
// asked to answer in the format, and nothing validates or repairs the answer,
// so callers that need a guaranteed schema must still validate the output.

// bpsOutputFormatContract returns the instruction for a text.format object,
// or "" for plain text.
func bpsOutputFormatContract(format gjson.Result) (string, error) {
	switch kind := format.Get("type").String(); kind {
	case "", "text":
		return "", nil
	case "json_object":
		return "Final answer format: return exactly one valid JSON object and nothing else. " +
			"Do not wrap it in Markdown code fences or add text before or after it.", nil
	case "json_schema":
		schema := format.Get("schema")
		if !schema.IsObject() {
			return "", bpsOutputFormatError("BPS 的 json_schema 输出格式需要 schema 对象")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(schema.Raw)); err != nil {
			return "", bpsOutputFormatError("BPS 的 json_schema 输出格式 schema 无效")
		}
		contract := "Final answer format: return exactly one valid JSON value that conforms to the JSON Schema below and nothing else. " +
			"Do not wrap it in Markdown code fences or add text before or after it. Include every required property and no properties the schema does not allow."
		if name := format.Get("name").String(); name != "" {
			contract += "\nSchema name: " + name
		}
		if description := format.Get("description").String(); description != "" {
			contract += "\nSchema description: " + description
		}
		return contract + "\nJSON Schema:\n" + compact.String(), nil
	default:
		if len(kind) > 64 {
			kind = kind[:64]
		}
		return "", bpsOutputFormatError(fmt.Sprintf("BPS 不支持 text.format 类型 %q", kind))
	}
}

func bpsOutputFormatError(message string) *Error {
	return &Error{Code: "invalid_request_error", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}
