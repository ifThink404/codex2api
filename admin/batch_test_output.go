package admin

import (
	"context"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const batchTestOutputLimit = 4 << 10

type batchTestOutputContextKey struct{}

type batchTestOutput struct {
	text               []byte
	truncated          bool
	model              string
	mode               string
	responseModel      string
	responseFieldCount *int
}

func batchTestOutputFromContext(ctx context.Context) *batchTestOutput {
	output, _ := ctx.Value(batchTestOutputContextKey{}).(*batchTestOutput)
	return output
}

func (output *batchTestOutput) append(text string) {
	if output == nil || output.truncated || text == "" {
		return
	}
	remaining := batchTestOutputLimit - len(output.text)
	if len(text) > remaining {
		text = text[:remaining]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		output.truncated = true
	}
	output.text = append(output.text, text...)
}

func (output *batchTestOutput) observeResponses(data []byte) {
	if output == nil {
		return
	}
	// Keep the model and top-level field count from the actual Responses object.
	// The requested test model may be an alias or may be rewritten upstream.
	response := gjson.GetBytes(data, "response")
	if !response.IsObject() && gjson.GetBytes(data, "object").String() == "response" {
		response = gjson.ParseBytes(data)
	}
	if response.IsObject() {
		if model := response.Get("model").String(); model != "" {
			output.responseModel = model
		}
		fields := response.Map()
		// Gateway billing metadata is added after inference; it is not evidence
		// of the upstream model's response shape.
		delete(fields, "codex2api_billing")
		fieldCount := len(fields)
		output.responseFieldCount = &fieldCount
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_text.delta":
		output.append(gjson.GetBytes(data, "delta").String())
	case "response.output_text.done":
		if len(output.text) == 0 {
			output.append(gjson.GetBytes(data, "text").String())
		}
	case "response.content_part.done":
		if len(output.text) == 0 {
			output.append(gjson.GetBytes(data, "part.text").String())
		}
	case "response.output_item.done":
		if len(output.text) == 0 {
			output.appendItem(gjson.GetBytes(data, "item"))
		}
	case "response.completed":
		completed := &batchTestOutput{}
		if text := gjson.GetBytes(data, "response.output_text").String(); text != "" {
			completed.append(text)
		} else {
			gjson.GetBytes(data, "response.output").ForEach(func(_, item gjson.Result) bool {
				completed.appendItem(item)
				return !completed.truncated
			})
		}
		if len(completed.text) > 0 {
			output.text, output.truncated = completed.text, completed.truncated
		}
	}
}

func (output *batchTestOutput) appendItem(item gjson.Result) {
	switch item.Get("type").String() {
	case "output_text", "text":
		output.append(item.Get("text").String())
	case "message", "assistant":
		item.Get("content").ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "output_text", "text":
				output.append(part.Get("text").String())
			}
			return !output.truncated
		})
	}
}
