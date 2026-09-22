package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const bpsRecentToolImages = 3
const bpsOmittedImageNote = "[历史图片已从本次请求省略。若原消息或对应工具调用保留了本地图片路径，可按需重新读取；路径失效或没有来源时无法保证恢复，请明确说明缺少原图。不要重新执行原浏览器点击等操作来代替旧截图。]"

// Request-local observations only: no image archive, retrieval tool, path,
// identifier, or image bytes are retained in this diagnostic.
type codexBPSImageHistoryDiagnostic struct {
	Policy               string   `json:"policy"`
	SkipReason           string   `json:"skip_reason,omitempty"`
	RecentToolImages     int      `json:"recent_tool_images"`
	Before               int      `json:"images_before"`
	After                int      `json:"images_after"`
	Omitted              int      `json:"images_omitted"`
	ReferenceBytesBefore int      `json:"reference_bytes_before"`
	ReferenceBytesAfter  int      `json:"reference_bytes_after"`
	InputBytesBefore     int      `json:"input_bytes_before"`
	InputBytesAfter      int      `json:"input_bytes_after"`
	Positions            []string `json:"positions,omitempty"`
	DetailsOmitted       int      `json:"details_omitted,omitempty"`
}

type bpsHistoryImage struct {
	item, part     int
	field          string
	tool, keep     bool
	referenceBytes int
}

// Trimming is a deterministic projection of the supplied history. It never
// changes client history or counts HTTP attempts as new observations. Images
// after the latest model item and the newest tool result remain intact, including
// parallel results or multi-image outputs. Re-reading an identical image in a
// new result therefore gets a new protected occurrence, without a hash cache.
func trimBPSImageHistory(items []json.RawMessage, body []byte, headers http.Header, compact bool, diagnostic *CodexBPSDiagnostic) ([]json.RawMessage, error) {
	d := &codexBPSImageHistoryDiagnostic{Policy: "recent_images_v1", RecentToolImages: bpsRecentToolImages}
	diagnostic.ImageHistory = d
	if compact {
		d.SkipReason = "compaction"
		return items, nil
	}
	root := resolveRequestRootSessionIdentity(headers, body)
	if root.conflict {
		d.SkipReason = "identity_conflict"
		return items, nil
	}
	if root.related || (root.threadSource != "" && root.threadSource != "user") || (root.requestKind != "" && root.requestKind != "turn") || strings.EqualFold(gjson.GetBytes(body, "model").String(), "codex-auto-review") {
		d.SkipReason = "non_user_request"
		return items, nil
	}
	controls := CodexRequestMetadataHeaders(headers, body)
	if controls.Get("X-OpenAI-Subagent") != "" || controls.Get("X-OpenAI-Memgen-Request") == "true" {
		d.SkipReason = "non_user_request"
		return items, nil
	}
	turn := gjson.Get(controls.Get(codexTurnMetadataHeader), "turn_id").String()
	if turn == "" {
		d.SkipReason = "missing_turn"
		return items, nil
	}
	lastModel, lastToolImageItem := -1, -1
	var images []bpsHistoryImage
	var toolIndices []int
	for i, raw := range items {
		d.InputBytesBefore += len(raw)
		item := gjson.ParseBytes(raw)
		kind, role := item.Get("type").String(), item.Get("role").String()
		if role == "assistant" || kind == "function_call" || kind == "custom_tool_call" || kind == "reasoning" {
			lastModel = i
		}
		field, tool := "", false
		switch {
		case (kind == "message" || kind == "") && role == "user":
			field = "content"
		case kind == "function_call_output" || kind == "custom_tool_call_output":
			field, tool = "output", true
		default:
			continue
		}
		if !item.Get(field).IsArray() {
			continue
		}
		metadata, _ := historyItemMetadata(item)
		itemTurn := gjson.ParseBytes(metadata["turn_id"]).String()
		for j, part := range item.Get(field).Array() {
			if part.Get("type").String() != "input_image" {
				continue
			}
			url, file := part.Get("image_url"), part.Get("file_id")
			// Unknown image shapes and ungrouped legacy history remain unchanged.
			known := url.Type == gjson.String && url.String() != "" || file.Type == gjson.String && file.String() != ""
			entry := bpsHistoryImage{item: i, part: j, field: field, tool: tool, keep: !known || itemTurn == "" || (!tool && itemTurn == turn), referenceBytes: len(url.String()) + len(file.String())}
			if tool {
				toolIndices = append(toolIndices, len(images))
				lastToolImageItem = i
			}
			images = append(images, entry)
		}
	}
	for n, index := range toolIndices {
		if n >= len(toolIndices)-bpsRecentToolImages || images[index].item == lastToolImageItem || images[index].item >= lastModel {
			images[index].keep = true
		}
	}
	// Preserve all unseen trailing images, including a newly appended user image.
	for i := range images {
		if images[i].item >= lastModel {
			images[i].keep = true
		}
	}
	out := append([]json.RawMessage(nil), items...)
	omitByItem := make(map[int][]bpsHistoryImage)
	for _, entry := range images {
		d.Before++
		d.ReferenceBytesBefore += entry.referenceBytes
		if entry.keep {
			d.After++
			d.ReferenceBytesAfter += entry.referenceBytes
			continue
		}
		omitByItem[entry.item] = append(omitByItem[entry.item], entry)
		d.Omitted++
		position := fmt.Sprintf("input[%d].%s[%d]", entry.item, entry.field, entry.part)
		if len(d.Positions) < 8 {
			d.Positions = append(d.Positions, position)
		} else {
			d.DetailsOmitted++
		}
		if diagnostic.Images != nil {
			for i := range diagnostic.Images.Details {
				v := &diagnostic.Images.Details[i]
				if v.Path == position {
					v.Action, v.OutboundReference = "history_omitted", "text_placeholder"
				}
			}
		}
	}
	// Marshal each affected item once; repeatedly rewriting a multi-megabyte
	// image array per image would multiply allocations by the number of images.
	note, _ := json.Marshal(map[string]string{"type": "input_text", "text": bpsOmittedImageNote})
	for index, omitted := range omitByItem {
		metadata, object := historyItemMetadata(gjson.ParseBytes(items[index]))
		field := omitted[0].field
		var parts []json.RawMessage
		if err := json.Unmarshal(object[field], &parts); err != nil {
			return nil, err
		}
		var kinds []json.RawMessage
		_ = json.Unmarshal(metadata["content_item_kinds"], &kinds)
		for _, entry := range omitted {
			parts[entry.part] = note
			// Never label a gateway-generated placeholder as a user instruction.
			if field == "content" && entry.part < len(kinds) {
				kinds[entry.part] = json.RawMessage(`"unknown"`)
			}
		}
		object[field], _ = json.Marshal(parts)
		if field == "content" && len(kinds) > 0 {
			metadata["content_item_kinds"], _ = json.Marshal(kinds)
			object["internal_chat_message_metadata_passthrough"], _ = json.Marshal(metadata)
		}
		var err error
		out[index], err = json.Marshal(object)
		if err != nil {
			return nil, err
		}
	}
	// Final image-reference counts are recorded after the attachment upload pass.
	for _, raw := range out {
		d.InputBytesAfter += len(raw)
	}
	if d.Omitted == 0 {
		return items, nil
	}
	return out, nil
}
