package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const bpsHistoryCurrentTurn = "01a0c75b-e1f7-7840-bf47-c14682a8780f"
const bpsHistoryOldTurn = "01a0be54-cc29-78b0-8816-15ca2e98576e"

func bpsHistoryTestBody(t *testing.T) []byte {
	t.Helper()
	image := map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + bpsTestPNG(t), "detail": "original"}
	metadata := func(turn string) map[string]any {
		return map[string]any{"turn_id": turn, "create_time": json.Number("1790051285.3378558")}
	}
	message := func(role, turn string, content []any) map[string]any {
		if role == "assistant" {
			for _, part := range content {
				p := part.(map[string]any)
				if p["type"] == "input_text" {
					p["type"] = "output_text"
				}
			}
		}
		return map[string]any{"type": "message", "role": role, "content": content, "internal_chat_message_metadata_passthrough": metadata(turn)}
	}
	text := func(s string) map[string]any { return map[string]any{"type": "input_text", "text": s} }
	old := message("user", bpsHistoryOldTurn, []any{text(`<image name="old" path="C:\private\old.png">`), image, text("</image>")})
	old["id"] = "msg_old"
	old["large_integer"] = json.Number("9007199254740993")
	old["internal_chat_message_metadata_passthrough"].(map[string]any)["content_item_kinds"] = []string{"user.text", "user.image", "user.text"}
	items := []any{old, message("assistant", bpsHistoryOldTurn, []any{text("Earlier description stays.")}), message("user", bpsHistoryCurrentTurn, []any{image, text("Current image stays.")})}
	for i := 0; i < 5; i++ {
		callID := fmt.Sprintf("call_history_%d", i)
		items = append(items, map[string]any{"type": "custom_tool_call", "call_id": callID, "name": "read_image", "input": fmt.Sprintf("image-%d", i)}, map[string]any{"type": "custom_tool_call_output", "id": fmt.Sprintf("result_%d", i), "call_id": callID, "output": []any{text("Tool text stays."), image}, "internal_chat_message_metadata_passthrough": metadata(bpsHistoryCurrentTurn)})
		if i < 4 {
			items = append(items, message("assistant", bpsHistoryCurrentTurn, []any{text("Continue.")}))
		}
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": items, "client_metadata": map[string]any{"session_id": testRootSessionA, "thread_id": testRootSessionA, "x-codex-turn-metadata": map[string]any{"session_id": testRootSessionA, "thread_id": testRootSessionA, "turn_id": bpsHistoryCurrentTurn, "thread_source": "user", "request_kind": "turn"}}})
	require.NoError(t, err)
	return body
}

func bpsCountHistoryImages(body []byte) int {
	n := 0
	for _, item := range gjson.GetBytes(body, "input").Array() {
		for _, field := range []string{"content", "output"} {
			for _, part := range item.Get(field).Array() {
				if part.Get("type").String() == "input_image" {
					n++
				}
			}
		}
	}
	return n
}

func TestBPSImageHistoryProjectionAndRetry(t *testing.T) {
	body := bpsHistoryTestBody(t)
	before := bytes.Clone(body)
	full, off, err := prepareCodexBPSBody(body, "stable", false)
	require.NoError(t, err)
	require.Nil(t, off.ImageHistory)
	require.Equal(t, 7, bpsCountHistoryImages(full))
	out, d, err := prepareCodexBPSBodyWithImageTrim(body, "stable", false, true, nil)
	require.NoError(t, err)
	require.Equal(t, before, body)
	require.Equal(t, 7, d.ImageHistory.Before)
	require.Equal(t, 4, d.ImageHistory.After)
	require.Equal(t, 3, d.ImageHistory.Omitted)
	require.Equal(t, 4, bpsCountHistoryImages(out))
	require.Zero(t, d.Images.InlineImages) // populated only after the upload pass
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "input.1.large_integer").Raw)
	require.Equal(t, "msg_old", gjson.GetBytes(out, "input.1.id").String())
	require.Equal(t, bpsOmittedImageNote, gjson.GetBytes(out, "input.1.content.1.text").String())
	require.Equal(t, "unknown", gjson.GetBytes(out, "input.1.internal_chat_message_metadata_passthrough.content_item_kinds.1").String())
	require.Contains(t, gjson.GetBytes(out, "input.1.content.0.text").String(), `C:\private\old.png`)
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if strings.HasPrefix(item.Get("id").String(), "result_") {
			require.Equal(t, "custom_tool_call_output", item.Get("type").String())
			require.Equal(t, "Tool text stays.", item.Get("output.0.text").String())
			require.NotEmpty(t, item.Get("call_id").String())
		}
	}
	again, _, err := prepareCodexBPSBodyWithImageTrim(body, "stable", false, true, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(out), string(again))
	logged, err := json.Marshal(d.ImageHistory)
	require.NoError(t, err)
	require.NotContains(t, string(logged), "private")
	require.NotContains(t, string(logged), "data:image")
	require.NotContains(t, string(out), "images_omitted")
}

func TestBPSImageHistoryWholeLatestGroupAndUnknownMetadata(t *testing.T) {
	body := bpsHistoryTestBody(t)
	items := gjson.GetBytes(body, "input").Array()
	last := len(items) - 1
	part := items[last].Get("output.1").Raw
	output := `[` + strings.TrimSuffix(strings.Repeat(part+",", 6), ",") + `]`
	body, err := sjson.SetRawBytes(body, fmt.Sprintf("input.%d.output", last), []byte(output))
	require.NoError(t, err)
	out, d, err := prepareCodexBPSBodyWithImageTrim(body, "stable", false, true, nil)
	require.NoError(t, err)
	require.Equal(t, 7, d.ImageHistory.After) // current user image + whole latest six-image result
	require.Len(t, gjson.GetBytes(out, fmt.Sprintf("input.%d.output", last+1)).Array(), 6)
	// Missing per-item labels use the visible user/model history boundary.
	body, err = sjson.DeleteBytes(body, "input.0.internal_chat_message_metadata_passthrough.turn_id")
	require.NoError(t, err)
	out, _, err = prepareCodexBPSBodyWithImageTrim(body, "stable", false, true, nil)
	require.NoError(t, err)
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.1.content.1.type").String())
	// Missing request turn still preserves the latest user segment and group.
	body, err = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-metadata.turn_id")
	require.NoError(t, err)
	out, d, err = prepareCodexBPSBodyWithImageTrim(body, "stable", false, true, nil)
	require.NoError(t, err)
	require.Empty(t, d.ImageHistory.SkipReason)
	require.Equal(t, "history_order", d.ImageHistory.BoundarySource)
	require.Equal(t, 7, bpsCountHistoryImages(out))
	require.Equal(t, 6, d.ImageHistory.RetainedReasons["latest_tool_group"])
	require.Equal(t, 1, d.ImageHistory.RetainedReasons["latest_user_segment"])
}

func TestBPSImageHistoryPassiveAndCompactUnchanged(t *testing.T) {
	for _, source := range []string{"guardian_review", "guardian_classifier", "thread_spawn", "memory_consolidation", "title", "future_background"} {
		t.Run(source, func(t *testing.T) {
			body, err := sjson.SetBytes(bpsHistoryTestBody(t), "client_metadata.x-codex-turn-metadata.thread_source", source)
			require.NoError(t, err)
			out, d, err := prepareCodexBPSBodyWithImageTrim(body, "stable", false, true, nil)
			require.NoError(t, err)
			require.NotEmpty(t, d.ImageHistory.SkipReason)
			require.Equal(t, 7, bpsCountHistoryImages(out))
		})
	}
	body := bpsHistoryTestBody(t)
	out, d, err := prepareCodexBPSBodyWithImageTrim(body, "stable", true, true, nil)
	require.NoError(t, err)
	require.Equal(t, "compaction", d.ImageHistory.SkipReason)
	require.Equal(t, 7, bpsCountHistoryImages(out))
	for _, kind := range []string{"compaction", "memory", "summarization"} {
		b, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.request_kind", kind)
		require.NoError(t, err)
		out, d, err = prepareCodexBPSBodyWithImageTrim(b, "stable", false, true, nil)
		require.NoError(t, err)
		require.NotEmpty(t, d.ImageHistory.SkipReason)
		require.Equal(t, 7, bpsCountHistoryImages(out))
	}
}

func TestBPSImageHistoryDoesNotTouchNonImageContent(t *testing.T) {
	body := bpsHistoryTestBody(t)
	items := gjson.GetBytes(body, "input").Array()
	var raw []json.RawMessage
	for _, item := range items {
		raw = append(raw, json.RawMessage(item.Raw))
	}
	// A text-encoded image object, file/audio/video content, and reasoning state
	// must remain byte-for-byte intact even beside an eligible old image.
	extra := json.RawMessage(`{"type":"function_call_output","call_id":"call_opaque","output":[{"type":"input_text","text":"{\"type\":\"input_image\",\"image_url\":\"do-not-rewrite\"}"},{"type":"input_file","file_data":"PRIVATE_FILE"},{"type":"input_audio","data":"PRIVATE_AUDIO"},{"type":"input_video","video_url":"PRIVATE_VIDEO"}]}`)
	raw = append(raw, extra)
	before := bytes.Clone(extra)
	d := &CodexBPSDiagnostic{}
	trimmed, err := trimBPSImageHistory(raw, body, nil, false, d)
	require.NoError(t, err)
	require.Positive(t, d.ImageHistory.Omitted)
	require.Equal(t, before, []byte(trimmed[len(trimmed)-1]))
	require.Equal(t, before, []byte(raw[len(raw)-1]))
}

func TestBPSImageHistoryFileReferencesAndPendingParallelResults(t *testing.T) {
	var items []json.RawMessage
	// Referenced old images can also be omitted, without fetching their files.
	for i := 0; i < 12; i++ {
		items = append(items, json.RawMessage(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":[{"type":"input_image","file_id":"file_old_%d"}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}`, i, i, bpsHistoryOldTurn)))
	}
	items = append(items, json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Read the next batch."}]}`))
	// Keep every image in a fresh parallel batch, even if it exceeds three.
	for i := 0; i < 5; i++ {
		items = append(items, json.RawMessage(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_new_%d","output":[{"type":"input_image","image_url":"https://example.invalid/image-%d.png"}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}`, i, i, bpsHistoryCurrentTurn)))
	}
	d := &CodexBPSDiagnostic{}
	out, err := trimBPSImageHistory(items, bpsHistoryTestBody(t), nil, false, d)
	require.NoError(t, err)
	require.Equal(t, 17, d.ImageHistory.Before)
	require.Equal(t, 5, d.ImageHistory.After)
	require.Equal(t, 12, d.ImageHistory.Omitted)
	require.Len(t, d.ImageHistory.Positions, 8)
	require.Equal(t, 4, d.ImageHistory.DetailsOmitted)
	for i := 0; i < 12; i++ {
		require.Equal(t, "input_text", gjson.GetBytes(out[i], "output.0.type").String())
		require.Equal(t, fmt.Sprintf("call_%d", i), gjson.GetBytes(out[i], "call_id").String())
	}
	for i := 13; i < len(items); i++ {
		require.Equal(t, items[i], out[i])
	}
	logged, err := json.Marshal(d.ImageHistory)
	require.NoError(t, err)
	require.NotContains(t, string(logged), "file_old")
	require.NotContains(t, string(logged), "example.invalid")
}

func TestBPSImageHistoryExecutorSkipsOldUpload(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "history.db"))
	require.NoError(t, err)
	defer db.Close()
	a := &auth.Account{DBID: 881125, AccountID: "history-fixture-account", AccessToken: "test-only", CodexBPS: true, CodexBPSImageTrim: true}
	body := bpsHistoryTestBody(t)
	// Keep only the old user image, a past reply, and a current text question.
	items := gjson.GetBytes(body, "input").Array()
	input := []json.RawMessage{json.RawMessage(items[0].Raw), json.RawMessage(items[1].Raw), json.RawMessage(`{"type":"message","role":"user","content":"Continue without a new image."}`)}
	encoded, _ := json.Marshal(input)
	body, err = sjson.SetRawBytes(body, "input", encoded)
	require.NoError(t, err)
	calls := 0
	installClaudeBoundaryTransport(t, a, func(req *http.Request) (*http.Response, error) {
		calls++
		require.True(t, strings.HasSuffix(req.URL.Path, "/responses"), "old image must be pruned before upload")
		wire, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Zero(t, bpsCountHistoryImages(wire))
		require.Contains(t, string(wire), bpsOmittedImageNote)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"resp_test","output":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n")), Request: req}, nil
	})
	ctx := WithCodexAccountTestIdentityStore(context.Background(), db, a)
	resp, err := ExecuteRequest(ctx, a, body, testRootSessionA, "", "", nil, nil, false)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, CodexBPSResponseDiagnostic(resp).ImageHistory.Omitted)
}

func TestBPSImageHistoryUploadsOnlyKeptReferences(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "kept-images.db"))
	require.NoError(t, err)
	defer db.Close()
	a := &auth.Account{DBID: 881126, AccountID: "kept-images-account", AccessToken: "test-only", CodexBPS: true, CodexBPSImageTrim: true}
	body := bpsHistoryTestBody(t)
	original := bytes.Clone(body)
	uploads, responses := 0, 0
	installClaudeBoundaryTransport(t, a, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/attachments") {
			uploads++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-kept-image"}`)), Request: req}, nil
		}
		responses++
		wire, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, 4, bpsCountHistoryImages(wire)) // current user + three recent tool images
		require.NotContains(t, string(wire), "data:image")
		require.Equal(t, 3, strings.Count(string(wire), bpsOmittedImageNote))
		for _, item := range gjson.GetBytes(wire, "input").Array() {
			for _, part := range item.Get("content").Array() {
				if part.Get("type").String() == "input_image" {
					require.Equal(t, "file-kept-image", part.Get("file_id").String())
				}
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: req}, nil
	})
	for round := 0; round < 2; round++ {
		ctx := WithCodexAccountTestIdentityStore(t.Context(), db, a)
		resp, err := ExecuteRequest(ctx, a, body, testRootSessionA, "", "", nil, nil, false)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		d := CodexBPSResponseDiagnostic(resp)
		require.Equal(t, 7, d.Images.Count)
		require.Equal(t, 3, d.ImageHistory.Omitted)
		require.Equal(t, 4, d.ImageHistory.After)
		require.Equal(t, 4, d.Images.Uploaded+d.Images.UploadReused)
		require.Zero(t, d.Images.InlineImages)
		require.Equal(t, 3, d.Images.ToolAttachmentMessages)
		for _, detail := range d.Images.Details {
			if detail.Action == "history_omitted" {
				require.Equal(t, "text_placeholder", detail.OutboundReference)
			} else {
				require.Equal(t, "file_id", detail.OutboundReference)
			}
		}
		require.Equal(t, original, body)
	}
	require.Equal(t, 1, uploads)
	require.Equal(t, 2, responses)
}
