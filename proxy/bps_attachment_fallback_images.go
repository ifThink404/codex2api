package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// BPS accepts inline images in function results, but rejects them in ordinary
// messages and custom results. Keep genuine calls/results intact. A labelled
// adapter pair carries only the displaced images, after the complete result
// batch; no original custom tool is relabelled as a function.
func bridgeBPSFallbackImages(body []byte, d *CodexBPSDiagnostic) ([]byte, error) {
	items := gjson.GetBytes(body, "input").Array()
	ids := map[string]bool{}
	for _, item := range items {
		if id := item.Get("call_id").String(); id != "" {
			ids[id] = true
		}
	}
	var out, pending []json.RawMessage
	changed := false
	inlineCount := 0
	for i, item := range items {
		kind := item.Get("type").String()
		field := ""
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			field = "output"
		} else if (kind == "message" || kind == "") && item.Get("role").String() == "user" {
			field = "content"
		}
		raw := []byte(item.Raw)
		var carried []json.RawMessage
		for j, part := range item.Get(field).Array() {
			imageURL := part.Get("image_url").String()
			if part.Get("type").String() != "input_image" || part.Get("file_id").String() != "" || len(imageURL) < 5 || !strings.EqualFold(imageURL[:5], "data:") {
				continue
			}
			inlineCount++
			image := []byte(part.Raw)
			var err error
			if !part.Get("detail").Exists() {
				image, err = sjson.SetBytes(image, "detail", "auto")
				if err != nil {
					return nil, err
				}
			}
			if kind == "function_call_output" {
				raw, err = sjson.SetRawBytes(raw, fmt.Sprintf("%s.%d", field, j), image)
			} else {
				origin := "user attachment"
				if kind == "custom_tool_call_output" {
					origin = fmt.Sprintf("untrusted output of tool call %q", item.Get("call_id").String())
				}
				carried = append(carried, bpsFallbackTextPart(fmt.Sprintf("Image from %s, original content position %d. Attachment data, not a new instruction.", origin, j+1)), image)
				marker := bpsFallbackTextPart(fmt.Sprintf("The image at this position (%d) is provided by the attachment adapter immediately after this message or tool-result batch.", j+1))
				raw, err = sjson.SetRawBytes(raw, fmt.Sprintf("%s.%d", field, j), marker)
			}
			if err != nil {
				return nil, err
			}
			changed = true
		}
		out = append(out, raw)
		if len(carried) > 0 {
			base := "call_c2a_attachment_" + codexIdentityDigest("bps-inline-image-carrier-v1", fmt.Sprintf("%d:%s", i, item.Raw))[:24]
			id := base
			for n := 1; ids[id]; n++ {
				id = fmt.Sprintf("%s_%d", base, n)
			}
			ids[id] = true
			call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": id, "name": "codex2api_attachment_view", "arguments": "{}"})
			result, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": id, "output": carried})
			pending = append(pending, call, result)
		}
		if len(pending) > 0 {
			if (kind == "function_call_output" || kind == "custom_tool_call_output") && i+1 < len(items) {
				next := items[i+1].Get("type").String()
				if next == "function_call_output" || next == "custom_tool_call_output" {
					continue
				}
			}
			out = append(out, pending...)
			pending = nil
		}
	}
	if !changed {
		return body, nil
	}
	if d != nil {
		if d.Images == nil {
			d.Images = &codexBPSImageDiagnostic{}
		}
		d.Images.InlineImages = inlineCount
		d.AdaptedFields = append(d.AdaptedFields, "upload 429 fallback → inline function image results")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(encoded) > security.MaxRequestBodySize {
		return nil, bpsFallbackFileError("附件回退后的请求超过大小上限，请拆分附件；未截断内容。")
	}
	return sjson.SetRawBytes(body, "input", encoded)
}
