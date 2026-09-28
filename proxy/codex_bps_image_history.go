package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const bpsRecentToolAttachments = 3
const bpsOmittedImageNote = "[历史图片已从本次请求省略。若原消息或对应工具调用保留了本地图片路径，可按需重新读取；路径失效或没有来源时无法保证恢复，请明确说明缺少原图。不要重新执行原浏览器点击等操作来代替旧截图。]"

// Request-local observations only: no attachment archive, retrieval tool, path,
// identifier, or image bytes are retained in this diagnostic.
// Keep the existing image_history envelope and image counters for old readers.
// The same saved switch now covers both images and files.
type codexBPSImageHistoryDiagnostic struct {
	RecentToolAttachments int      `json:"recent_tool_attachments"`
	AttachmentsBefore     int      `json:"attachments_before"`
	AttachmentsAfter      int      `json:"attachments_after"`
	AttachmentsOmitted    int      `json:"attachments_omitted"`
	FilesBefore           int      `json:"files_before"`
	FilesAfter            int      `json:"files_after"`
	FilesOmitted          int      `json:"files_omitted"`
	Policy                string   `json:"policy"`
	SkipReason            string   `json:"skip_reason,omitempty"`
	RecentToolImages      int      `json:"recent_tool_images"`
	Before                int      `json:"images_before"`
	After                 int      `json:"images_after"`
	Omitted               int      `json:"images_omitted"`
	ReferenceBytesBefore  int      `json:"reference_bytes_before"`
	ReferenceBytesAfter   int      `json:"reference_bytes_after"`
	InputBytesBefore      int      `json:"input_bytes_before"`
	InputBytesAfter       int      `json:"input_bytes_after"`
	Positions             []string `json:"positions,omitempty"`
	DetailsOmitted        int      `json:"details_omitted,omitempty"`
}

type bpsHistoryAttachment struct {
	file           bool
	item, part     int
	field          string
	tool, keep     bool
	referenceBytes int
}

// Trimming is a deterministic projection of the supplied history. It never
// changes client history or counts HTTP attempts as new observations. Images
// and files share the three recent tool slots. Current user attachments, the
// newest complete result and unseen parallel results remain intact. Re-reading
// a file in a new result gets a protected occurrence without a hash cache.
func trimBPSImageHistory(items []json.RawMessage, body []byte, headers http.Header, compact bool, diagnostic *CodexBPSDiagnostic) ([]json.RawMessage, error) {
	d := &codexBPSImageHistoryDiagnostic{Policy: "recent_attachments_v1", RecentToolImages: bpsRecentToolAttachments, RecentToolAttachments: bpsRecentToolAttachments}
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
	lastModel, lastToolAttachmentItem := -1, -1
	var attachments []bpsHistoryAttachment
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
			kind := part.Get("type").String()
			if kind != "input_image" && kind != "input_file" {
				continue
			}
			known, referenceBytes := bpsHistoryAttachmentReference(part)
			// Unknown shapes and ungrouped legacy history remain unchanged.
			entry := bpsHistoryAttachment{item: i, part: j, field: field, tool: tool, file: kind == "input_file", keep: !known || itemTurn == "" || (!tool && itemTurn == turn), referenceBytes: referenceBytes}
			if tool {
				toolIndices = append(toolIndices, len(attachments))
				lastToolAttachmentItem = i
			}
			attachments = append(attachments, entry)
		}
	}
	for n, index := range toolIndices {
		if n >= len(toolIndices)-bpsRecentToolAttachments || attachments[index].item == lastToolAttachmentItem || attachments[index].item >= lastModel {
			attachments[index].keep = true
		}
	}
	// Preserve all unseen trailing attachments, including newly appended user files.
	for i := range attachments {
		if attachments[i].item >= lastModel {
			attachments[i].keep = true
		}
	}
	out := append([]json.RawMessage(nil), items...)
	omitByItem := make(map[int][]bpsHistoryAttachment)
	for _, entry := range attachments {
		d.AttachmentsBefore++
		if entry.file {
			d.FilesBefore++
		} else {
			d.Before++
		}
		d.ReferenceBytesBefore += entry.referenceBytes
		if entry.keep {
			d.AttachmentsAfter++
			if entry.file {
				d.FilesAfter++
			} else {
				d.After++
			}
			d.ReferenceBytesAfter += entry.referenceBytes
			continue
		}
		omitByItem[entry.item] = append(omitByItem[entry.item], entry)
		d.AttachmentsOmitted++
		if entry.file {
			d.FilesOmitted++
		} else {
			d.Omitted++
		}
		position := fmt.Sprintf("input[%d].%s[%d]", entry.item, entry.field, entry.part)
		if len(d.Positions) < 8 {
			d.Positions = append(d.Positions, position)
		} else {
			d.DetailsOmitted++
		}
		if !entry.file && diagnostic.Images != nil {
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
			parts[entry.part] = bpsOmittedAttachmentPart(gjson.ParseBytes(parts[entry.part]))
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
	if d.AttachmentsOmitted == 0 {
		return items, nil
	}
	return out, nil
}
