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
	RecentToolAttachments int            `json:"recent_tool_attachments"`
	AttachmentsBefore     int            `json:"attachments_before"`
	AttachmentsAfter      int            `json:"attachments_after"`
	AttachmentsOmitted    int            `json:"attachments_omitted"`
	FilesBefore           int            `json:"files_before"`
	FilesAfter            int            `json:"files_after"`
	FilesOmitted          int            `json:"files_omitted"`
	Policy                string         `json:"policy"`
	SkipReason            string         `json:"skip_reason,omitempty"`
	BoundarySource        string         `json:"boundary_source,omitempty"`
	RetainedReasons       map[string]int `json:"retained_reasons,omitempty"`
	RecentToolImages      int            `json:"recent_tool_images"`
	Before                int            `json:"images_before"`
	After                 int            `json:"images_after"`
	Omitted               int            `json:"images_omitted"`
	ReferenceBytesBefore  int            `json:"reference_bytes_before"`
	ReferenceBytesAfter   int            `json:"reference_bytes_after"`
	InputBytesBefore      int            `json:"input_bytes_before"`
	InputBytesAfter       int            `json:"input_bytes_after"`
	Positions             []string       `json:"positions,omitempty"`
	DetailsOmitted        int            `json:"details_omitted,omitempty"`
}

type bpsHistoryAttachment struct {
	file           bool
	item, part     int
	field          string
	tool           bool
	keepReason     string
	turn           string
	referenceBytes int
}

// Being related to a root session describes routing, not whether this is a
// passive request. Ordinary forks and explicitly identified worker turns use
// the same history policy; review, memory and unknown background roles do not.
func bpsHistorySkipReason(root requestRootSessionIdentity, controls http.Header, body []byte, compact bool) string {
	if compact {
		return "compaction"
	}
	if root.conflict {
		return "identity_conflict"
	}
	if (root.requestKind != "" && root.requestKind != "turn") || strings.EqualFold(gjson.GetBytes(body, "model").String(), "codex-auto-review") || controls.Get("X-OpenAI-Memgen-Request") == "true" {
		return "non_user_request"
	}
	worker := false
	for _, kind := range []string{root.subagentKind, controls.Get("X-OpenAI-Subagent")} {
		switch kind {
		case "":
		case "thread_spawn", "collab_spawn":
			worker = true
		default:
			return "non_user_request"
		}
	}
	switch root.threadSource {
	case "":
		if root.related && !worker {
			return "non_user_request"
		}
		return ""
	case "user":
		return ""
	case "subagent":
		if worker {
			return ""
		}
	}
	return "non_user_request"
}

// Trimming is a deterministic projection of the supplied history. It never
// changes client history or counts HTTP attempts as new observations. Images
// and files share the three recent tool slots. Current user attachments, the
// newest complete result and unseen parallel results remain intact. Re-reading
// a file in a new result gets a protected occurrence without a hash cache.
func trimBPSImageHistory(items []json.RawMessage, body []byte, headers http.Header, compact bool, diagnostic *CodexBPSDiagnostic) ([]json.RawMessage, error) {
	d := &codexBPSImageHistoryDiagnostic{Policy: "recent_attachments_v2", RecentToolImages: bpsRecentToolAttachments, RecentToolAttachments: bpsRecentToolAttachments}
	diagnostic.ImageHistory = d
	root := resolveRequestRootSessionIdentity(headers, body)
	controls := CodexRequestMetadataHeaders(headers, body)
	d.SkipReason = bpsHistorySkipReason(root, controls, body, compact)
	turn := gjson.Get(controls.Get(codexTurnMetadataHeader), "turn_id").String()
	if d.SkipReason == "" {
		d.BoundarySource = "history_order"
		if turn != "" {
			d.BoundarySource = "turn_metadata_and_history"
		}
	}
	lastModel, modelBeforeLatestUser, lastToolAttachmentItem := -1, -1, -1
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
			// Preserve the whole latest user segment, including multiple message
			// fragments. A newer user message alone is not proof of a new turn;
			// a model item between them establishes the history boundary.
			modelBeforeLatestUser = lastModel
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
			entry := bpsHistoryAttachment{item: i, part: j, field: field, tool: tool, file: kind == "input_file", turn: itemTurn, referenceBytes: referenceBytes}
			if !known {
				entry.keepReason = "unknown_shape"
			}
			if tool {
				toolIndices = append(toolIndices, len(attachments))
				lastToolAttachmentItem = i
			}
			attachments = append(attachments, entry)
		}
	}
	for n, index := range toolIndices {
		if attachments[index].keepReason == "" && n >= len(toolIndices)-bpsRecentToolAttachments {
			attachments[index].keepReason = "recent_tool"
		}
	}
	for i := range attachments {
		entry := &attachments[i]
		switch {
		case d.SkipReason != "":
			entry.keepReason = "policy_skipped"
		case entry.keepReason == "unknown_shape":
		case !entry.tool && turn != "" && entry.turn == turn:
			entry.keepReason = "current_user_turn"
		case !entry.tool && (turn == "" || entry.turn == "") && entry.item > modelBeforeLatestUser:
			entry.keepReason = "latest_user_segment"
		case entry.tool && entry.item == lastToolAttachmentItem:
			entry.keepReason = "latest_tool_group"
		case entry.item >= lastModel:
			entry.keepReason = "unseen_result"
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
		if entry.keepReason != "" {
			if d.RetainedReasons == nil {
				d.RetainedReasons = make(map[string]int)
			}
			d.RetainedReasons[entry.keepReason]++
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
