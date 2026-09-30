package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Keep only a bounded structural summary. Image bytes, URLs, text and tool
// arguments must never be copied into the local compatibility diagnostic.
type codexBPSImageDiagnostic struct {
	Count                  int                   `json:"count"`
	MIMENormalized         int                   `json:"mime_normalized"`
	DetailNormalized       int                   `json:"detail_normalized,omitempty"`
	Uploaded               int                   `json:"uploaded,omitempty"`
	UploadReused           int                   `json:"upload_reused,omitempty"`
	ToolOutputConversions  int                   `json:"tool_output_conversions,omitempty"`
	InlineImages           int                   `json:"inline_images"`
	ToolAttachmentMessages int                   `json:"tool_attachment_messages,omitempty"`
	DetailsOmitted         int                   `json:"details_omitted,omitempty"`
	OmittedAfterRefusal    int                   `json:"omitted_after_refusal,omitempty"`
	Details                []codexBPSImageDetail `json:"details"`
}

type codexBPSImageDetail struct {
	Path              string `json:"path"`
	Carrier           string `json:"carrier"`
	ItemType          string `json:"item_type,omitempty"`
	OutboundItemType  string `json:"outbound_item_type,omitempty"`
	Detail            string `json:"detail,omitempty"`
	Role              string `json:"role,omitempty"`
	Reference         string `json:"reference"`
	URLBytes          int    `json:"url_bytes,omitempty"`
	DeclaredMIME      string `json:"declared_mime,omitempty"`
	DetectedMIME      string `json:"detected_mime,omitempty"`
	Action            string `json:"action"`
	OutboundReference string `json:"outbound_reference,omitempty"`
}

// Only visit protocol image content, never JSON embedded in business strings,
// tool parameters, schemas, signatures or encrypted history. Keep the newest
// images in the diagnostic so an old long history cannot hide the failing item.
func normalizeBPSInputImages(items []json.RawMessage) ([]json.RawMessage, *codexBPSImageDiagnostic) {
	var diagnostic *codexBPSImageDiagnostic
	for index, raw := range items {
		item := gjson.ParseBytes(raw)
		field, carrier := "", ""
		switch item.Get("type").String() {
		case "message", "":
			if role := item.Get("role").String(); role != "user" && role != "assistant" && role != "developer" && role != "system" {
				continue
			}
			field, carrier = "content", "message_content"
		case "function_call_output", "custom_tool_call_output":
			field, carrier = "output", "tool_output"
		default:
			continue
		}
		content := item.Get(field)
		if !content.IsArray() {
			continue
		}
		for partIndex, part := range content.Array() {
			if part.Get("type").String() != "input_image" {
				continue
			}
			path := fmt.Sprintf("%s.%d.image_url", field, partIndex)
			detail := codexBPSImageDetail{Path: fmt.Sprintf("input[%d].%s[%d]", index, field, partIndex), Carrier: carrier, Reference: "unknown", Action: "preserved"}
			detail.ItemType = item.Get("type").String()
			if value := part.Get("detail"); value.Exists() {
				switch value.String() {
				case "auto", "low", "high", "original":
					detail.Detail = value.String()
				default:
					detail.Detail = "other"
				}
			}
			// BPS only accepts auto/low/high. "original" is the client's fidelity
			// hint; map it to the closest tier and keep the image bytes untouched.
			if detail.Detail == "original" {
				if updated, err := sjson.SetBytes(raw, fmt.Sprintf("%s.%d.detail", field, partIndex), "high"); err == nil {
					raw = updated
					detail.Action = "detail_normalized"
				}
			}
			if field == "content" {
				detail.Role = item.Get("role").String()
			}
			if imageURL := part.Get("image_url"); imageURL.Type == gjson.String {
				original := imageURL.String()
				detail.URLBytes = len(original)
				normalized := normalizeBPSImageDataURL(original, &detail)
				if normalized != original {
					if updated, err := sjson.SetBytes(raw, path, normalized); err == nil {
						raw = updated
						detail.Action = "mime_normalized"
					}
				}
			} else if part.Get("file_id").Type == gjson.String {
				detail.Reference = "file_id"
			}
			if diagnostic == nil {
				diagnostic = &codexBPSImageDiagnostic{}
			}
			diagnostic.Count++
			if detail.Action == "mime_normalized" {
				diagnostic.MIMENormalized++
			}
			if detail.Detail == "original" {
				diagnostic.DetailNormalized++
			}
			if len(diagnostic.Details) == 8 {
				diagnostic.Details = append(diagnostic.Details[:0], diagnostic.Details[1:]...)
				diagnostic.DetailsOmitted++
			}
			diagnostic.Details = append(diagnostic.Details, detail)
		}
		items[index] = raw
	}
	return items, diagnostic
}

func normalizeBPSImageDataURL(value string, detail *codexBPSImageDetail) string {
	if len(value) < 5 || !strings.EqualFold(value[:5], "data:") {
		detail.Reference = "url"
		return value
	}
	detail.Reference = "data_url"
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		detail.Action = "invalid_data_url"
		return value
	}
	metadata, encoded := value[5:comma], value[comma+1:]
	declared, _, _ := strings.Cut(metadata, ";")
	switch strings.ToLower(declared) {
	case "", "application/octet-stream", "image/png", "image/jpeg", "image/jpg", "image/webp", "image/gif", "image/bmp", "image/avif", "image/svg+xml", "text/plain":
		detail.DeclaredMIME = strings.ToLower(declared)
	default:
		detail.DeclaredMIME = "other"
	}
	if !strings.HasSuffix(strings.ToLower(metadata), ";base64") {
		detail.Action = "unsupported_data_encoding"
		return value
	}
	// Sniff a bounded prefix instead of decoding or re-encoding megabytes of
	// image data. The entire original Base64 suffix and image detail survive.
	var prefix [512]byte
	n, err := io.ReadFull(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)), prefix[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		detail.Action = "invalid_base64"
		return value
	}
	if n == 0 {
		detail.Action = "invalid_base64"
		return value
	}
	mime := http.DetectContentType(prefix[:n])
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		detail.DetectedMIME = mime
	default:
		detail.Action = "unknown_image_format"
		return value
	}
	return "data:" + mime + ";base64," + encoded
}

// Validate custom image results before sending. Data URLs are uploaded later
// and use the attachment-message bridge, preserving the custom result type.
// Only remote image URLs still need the function-result compatibility carrier;
// never fetch an arbitrary remote URL on the gateway's behalf.
func projectBPSCustomImageOutputs(items []json.RawMessage, d *CodexBPSDiagnostic) ([]json.RawMessage, error) {
	for i, raw := range items {
		item := gjson.ParseBytes(raw)
		if item.Get("type").String() != "custom_tool_call_output" || !item.Get("output").IsArray() {
			continue
		}
		imageCount, inlineCount := 0, 0
		for _, part := range item.Get("output").Array() {
			if part.Get("type").String() != "input_image" {
				continue
			}
			if part.Get("file_id").String() != "" {
				continue
			}
			value := part.Get("image_url").String()
			if len(value) < 5 || !strings.EqualFold(value[:5], "data:") {
				imageCount++
				continue
			}
			// Preserve the former validation without allocating an entire decoded
			// image. MIME normalization has already retained the original suffix.
			detail := codexBPSImageDetail{}
			normalized := normalizeBPSImageDataURL(value, &detail)
			if detail.DetectedMIME == "" {
				return nil, bpsImageInputError("图片数据无效或格式不受支持，请使用 PNG、JPEG、GIF 或 WebP 图片。")
			}
			_, encoded, _ := strings.Cut(normalized, ",")
			if len(encoded) > security.MaxRequestBodySize {
				return nil, bpsImageInputError("图片数据超过请求大小限制。")
			}
			if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))); err != nil {
				return nil, bpsImageInputError("图片 Base64 数据不完整或无效，请重新附加图片。")
			}
			inlineCount++
		}
		if imageCount+inlineCount == 0 {
			continue
		}
		if strings.TrimSpace(item.Get("call_id").String()) == "" {
			return nil, bpsImageInputError("图片工具结果缺少 call_id，无法关联原工具调用。")
		}
		if imageCount == 0 {
			continue
		}
		updated, err := sjson.SetBytes(raw, "type", "function_call_output")
		if err != nil {
			return nil, err
		}
		items[i] = updated
		if d != nil && d.Images != nil {
			d.Images.ToolOutputConversions++
			prefix := fmt.Sprintf("input[%d].output[", i)
			for j := range d.Images.Details {
				v := &d.Images.Details[j]
				if strings.HasPrefix(v.Path, prefix) && v.Reference == "url" {
					v.OutboundItemType = "function_call_output"
					v.OutboundReference = v.Reference
					v.Action = "tool_output_projected"
				}
			}
			if !slices.Contains(d.AdaptedFields, "custom remote image results → function_call_output (URLs preserved)") {
				d.AdaptedFields = append(d.AdaptedFields, "custom remote image results → function_call_output (URLs preserved)")
			}
		}
	}
	return items, nil
}
