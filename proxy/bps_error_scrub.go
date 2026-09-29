package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Provider error scrubbing (adopted from upstream's official Excel BPS
// adapter). BPS error bodies and failed stream events can echo request
// content, connector arguments or credential metadata, so provider text never
// crosses the gateway boundary: an error keeps only its enum-like type and
// code and the reset hints core schedules by, with a fixed message. Operators
// get the code/type shape in the log; sampled plugin captures keep the masked
// original, since they record the response before the transformer runs.

const (
	bpsScrubbedErrorMessage      = "BPS upstream returned an unsuccessful response"
	bpsScrubbedIncompleteMessage = "BPS upstream ended the response before completion"
)

// bpsErrorEnum keeps a provider code or type only when it is enum-like.
func bpsErrorEnum(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return ""
		}
	}
	return value
}

// bpsScrubbedError rebuilds one provider error object from source (the
// error object itself) and fallback (where reset hints may also live).
func bpsScrubbedError(source gjson.Result, message string, fallback ...gjson.Result) map[string]any {
	scrubbed := map[string]any{"message": message}
	if kind := bpsErrorEnum(source.Get("type").String()); kind != "" {
		scrubbed["type"] = kind
	}
	if code := bpsErrorEnum(source.Get("code").String()); code != "" {
		scrubbed["code"] = code
	}
	for _, key := range []string{"resets_at", "resets_in_seconds"} {
		for _, candidate := range append([]gjson.Result{source}, fallback...) {
			if value := candidate.Get(key); value.Type == gjson.Number && value.Int() > 0 {
				scrubbed[key] = value.Int()
				break
			}
		}
	}
	return scrubbed
}

// bpsErrorShape describes a provider error for operator logs using only its
// enum-like code and type and its size.
func bpsErrorShape(source gjson.Result, size int) string {
	return fmt.Sprintf("code=%q type=%q bytes=%d", bpsErrorEnum(source.Get("code").String()), bpsErrorEnum(source.Get("type").String()), size)
}

// scrubBPSErrorBody replaces a non-2xx BPS body with a scrubbed error
// envelope that keeps the status-bearing fields core classifies by.
func scrubBPSErrorBody(status int, body []byte) []byte {
	root := gjson.ParseBytes(body)
	source := bpsErrorBodySource(root)
	if !source.IsObject() {
		source = root
	}
	log.Printf("[bps] upstream HTTP %d: %s", status, bpsErrorShape(source, len(body)))
	scrubbed, err := json.Marshal(map[string]any{"error": bpsScrubbedError(source, bpsScrubbedErrorMessage)})
	if err != nil {
		return []byte(`{"error":{"message":"` + bpsScrubbedErrorMessage + `"}}`)
	}
	return scrubbed
}

// scrubBPSTerminalEvent removes provider error text from failed, incomplete
// and error stream events. Other events are returned unchanged.
func scrubBPSTerminalEvent(data []byte) ([]byte, error) {
	event := gjson.ParseBytes(data)
	kind := event.Get("type").String()
	status := strings.ToLower(strings.TrimSpace(event.Get("response.status").String()))
	switch {
	case kind == "error":
		log.Printf("[bps] upstream error event: %s", bpsErrorShape(bpsEventErrorSource(event), len(data)))
		scrubbed := bpsScrubbedError(bpsEventErrorSource(event), bpsScrubbedErrorMessage)
		out, err := sjson.DeleteBytes(data, "param")
		if err == nil {
			out, err = sjson.SetBytes(out, "message", bpsScrubbedErrorMessage)
		}
		if err == nil && event.Get("code").Exists() {
			out, err = sjson.SetBytes(out, "code", scrubbed["code"])
		}
		if err == nil && event.Get("error").Exists() {
			out, err = sjson.SetBytes(out, "error", scrubbed)
		}
		return out, err
	case kind == "response.failed" || kind == "response.incomplete" || kind == "response.completed" && (status == "failed" || status == "incomplete"):
		response := event.Get("response")
		if !response.Get("error").Exists() && !response.Get("status_details").Exists() {
			return data, nil
		}
		message := bpsScrubbedErrorMessage
		if kind == "response.incomplete" || status == "incomplete" {
			message = bpsScrubbedIncompleteMessage
		}
		source := response.Get("error")
		if !source.IsObject() {
			source = response.Get("status_details.error")
		}
		log.Printf("[bps] upstream %s: %s", kind, bpsErrorShape(source, len(data)))
		out, err := sjson.DeleteBytes(data, "response.status_details")
		if err == nil && (response.Get("error").Type != gjson.Null || source.Exists()) {
			out, err = sjson.SetBytes(out, "response.error", bpsScrubbedError(source, message, response.Get("status_details.error")))
		}
		return out, err
	}
	return data, nil
}

// bpsEventErrorSource is the error object of a top-level error event.
func bpsEventErrorSource(event gjson.Result) gjson.Result {
	if source := event.Get("error"); source.IsObject() {
		return source
	}
	return event
}

// bpsOriginalErrorMessage is the provider's own error text for the usage log:
// code, type and message of the error object, or the raw body when it has no
// message. It is never sent to the client.
func bpsOriginalErrorMessage(source gjson.Result, raw []byte) string {
	parts := make([]string, 0, 3)
	code, kind := strings.TrimSpace(source.Get("code").String()), strings.TrimSpace(source.Get("type").String())
	if code != "" {
		parts = append(parts, code)
	}
	if kind != "" && kind != code && kind != "error" {
		parts = append(parts, kind)
	}
	message := strings.TrimSpace(source.Get("message").String())
	if message == "" && source.Type == gjson.String {
		message = strings.TrimSpace(source.String())
	}
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if message != "" {
		parts = append(parts, message)
	}
	return security.SafeTruncate(strings.Join(parts, " · "), usageLogErrorMessageMaxRunes)
}

// bpsErrorBodySource is the error object of a non-2xx BPS body.
func bpsErrorBodySource(root gjson.Result) gjson.Result {
	for _, path := range []string{"error", "detail.error.error", "detail.error"} {
		if source := root.Get(path); source.IsObject() {
			return source
		}
	}
	if detail := root.Get("detail"); detail.Type == gjson.String {
		return detail
	}
	return root
}

// bpsTerminalEventSource returns the error object of a failed, incomplete or
// error stream event, and false for any other event.
func bpsTerminalEventSource(event gjson.Result) (gjson.Result, bool) {
	kind := event.Get("type").String()
	status := strings.ToLower(strings.TrimSpace(event.Get("response.status").String()))
	switch {
	case kind == "error":
		return bpsEventErrorSource(event), true
	case kind == "response.failed" || kind == "response.incomplete" || kind == "response.completed" && (status == "failed" || status == "incomplete"):
		response := event.Get("response")
		if source := response.Get("error"); source.IsObject() {
			return source, true
		}
		if source := response.Get("status_details.error"); source.IsObject() {
			return source, true
		}
		return response.Get("incomplete_details"), true
	}
	return gjson.Result{}, false
}
