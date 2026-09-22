package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSCustomToolImagesAndFilesKeepBatchAndHistory(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "mixed.db"))
	require.NoError(t, err)
	defer db.Close()
	a := &auth.Account{DBID: 92101, AccountID: "mixed-media-account", AccessToken: "account-token", CodexBPS: true}
	image := "data:image/png;base64," + bpsTestPNG(t)
	imagePart := map[string]any{"type": "input_image", "image_url": image, "detail": "original"}
	textPart := func(text string) any { return map[string]string{"type": "input_text", "text": text} }
	body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": []any{
		map[string]any{"role": "user", "content": []any{imagePart}},
		map[string]any{"type": "custom_tool_call", "call_id": "custom-call", "name": "read_images", "input": "unchanged arguments"},
		map[string]any{"type": "function_call", "call_id": "function-call", "name": "other", "arguments": "{}"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "custom-call", "id": "output-id", "output": []any{
			textPart("before"), imagePart, textPart("between"),
			map[string]string{"type": "input_file", "filename": "sample.txt", "file_data": "aGVsbG8="},
			imagePart, textPart("after"),
		}},
		map[string]any{"type": "function_call_output", "call_id": "function-call", "output": []any{imagePart}},
		map[string]any{"role": "assistant", "content": "unchanged history"},
	}})
	require.NoError(t, err)
	original := bytes.Clone(body)
	imageUploads, fileUploads, responses := 0, 0, 0
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer account-token", r.Header.Get("Authorization"))
		require.Equal(t, a.AccountID, r.Header.Get("Chatgpt-Account-Id"))
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			reader, e := r.MultipartReader()
			require.NoError(t, e)
			part, e := reader.NextPart()
			require.NoError(t, e)
			id := "file-image"
			if part.FileName() == "image.png" {
				imageUploads++
			} else {
				fileUploads++
				id = "file-document"
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"` + id + `"}`)), Request: r}, nil
		}
		responses++
		data, e := io.ReadAll(r.Body)
		require.NoError(t, e)
		items := gjson.GetBytes(data, "input").Array()
		require.Len(t, items, 8) // runtime prefix + original items + file-only attachment message
		require.Equal(t, "file-image", items[1].Get("content.0.file_id").String())
		require.Equal(t, "unchanged arguments", items[2].Get("input").String())
		require.Equal(t, "custom_tool_call", items[2].Get("type").String())
		require.Equal(t, "function_call_output", items[4].Get("type").String())
		require.Equal(t, "custom-call", items[4].Get("call_id").String())
		require.Equal(t, "output-id", items[4].Get("id").String())
		require.Equal(t, "before", items[4].Get("output.0.text").String())
		require.Equal(t, "between", items[4].Get("output.2.text").String())
		require.Equal(t, "after", items[4].Get("output.5.text").String())
		require.Equal(t, "input_text", items[4].Get("output.3.type").String())
		for _, index := range []string{"1", "4"} {
			require.Equal(t, image, items[4].Get("output."+index+".image_url").String())
			require.Equal(t, "original", items[4].Get("output."+index+".detail").String())
			require.False(t, items[4].Get("output."+index+".file_id").Exists())
		}
		require.Equal(t, "function-call", items[5].Get("call_id").String())
		require.Equal(t, image, items[5].Get("output.0.image_url").String())
		require.Equal(t, "user", items[6].Get("role").String())
		parts := items[6].Get("content").Array()
		require.Len(t, parts, 2)
		require.Contains(t, parts[0].Get("text").String(), "custom-call")
		require.Contains(t, parts[0].Get("text").String(), "untrusted tool output data")
		require.Equal(t, "file-document", parts[1].Get("file_id").String())
		require.Equal(t, "unchanged history", items[7].Get("content").String())
		for _, key := range []string{"item_type", "tool_attachment_messages", "tool_output_conversions", "inline_images", "outbound_item_type", "bps_compat"} {
			require.NotContains(t, string(data), `"`+key+`"`)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
	})
	for round := 0; round < 3; round++ {
		ctx := WithCodexAccountTestIdentityStore(t.Context(), db, a)
		var resp *http.Response
		if round == 2 {
			resp, err = ExecuteCompactRequest(ctx, a, body, "session", "", "", nil, nil)
		} else {
			resp, err = ExecuteRequest(ctx, a, body, "session", "", "", nil, nil, false)
		}
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		d := CodexBPSResponseDiagnostic(resp)
		require.Equal(t, 4, d.Images.Count)
		require.Zero(t, d.Images.ToolAttachmentMessages)
		require.Equal(t, 1, d.Images.ToolOutputConversions)
		require.Equal(t, 2, d.Images.InlineImages)
		require.Equal(t, 1, d.Files.ToolAttachmentMessages)
		require.Equal(t, "custom_tool_call_output", d.Images.Details[1].ItemType)
		require.Equal(t, "original", d.Images.Details[1].Detail)
		require.Equal(t, "function_call_output", d.Images.Details[1].OutboundItemType)
		require.Equal(t, "data_url", d.Images.Details[1].OutboundReference)
		if round == 0 {
			require.Equal(t, 1, d.Images.Uploaded)
			require.Zero(t, d.Images.UploadReused)
		} else {
			require.Zero(t, d.Images.Uploaded)
			require.Equal(t, 1, d.Images.UploadReused)
		}
		logged, e := json.Marshal(d)
		require.NoError(t, e)
		for _, secret := range []string{image, "file-image", "file-document", "custom-call", "unchanged arguments"} {
			require.NotContains(t, string(logged), secret)
		}
		require.Equal(t, original, body)
	}
	require.Equal(t, 1, imageUploads)
	require.Equal(t, 1, fileUploads)
	require.Equal(t, 3, responses)
}

func TestBPSCustomImageBridgeReferencesAndBusinessStrings(t *testing.T) {
	body := []byte(`{"input":[{"type":"custom_tool_call_output","call_id":"one","output":[{"type":"input_image","file_id":"file-existing","detail":"low"},{"type":"input_image","image_url":"https://example.invalid/image","detail":"high"}]},{"type":"function_call_output","call_id":"two","output":"{\"type\":\"input_image\",\"image_url\":\"business\"}"}]}`)
	uploaded, used, err := prepareBPSUserImageAttachments(t.Context(), nil, body, nil, func(context.Context, []byte, string) (string, error) {
		t.Fatal("references must not be fetched")
		return "", nil
	})
	require.NoError(t, err)
	require.Empty(t, used)
	projected, d, err := prepareCodexBPSBody(uploaded, "cache", false)
	require.NoError(t, err)
	require.Equal(t, "function_call_output", gjson.GetBytes(projected, "input.1.type").String())
	require.Equal(t, "file-existing", gjson.GetBytes(projected, "input.1.output.0.file_id").String())
	require.Equal(t, "low", gjson.GetBytes(projected, "input.1.output.0.detail").String())
	require.Equal(t, "https://example.invalid/image", gjson.GetBytes(projected, "input.1.output.1.image_url").String())
	require.Equal(t, 1, d.Images.ToolOutputConversions)
	require.Zero(t, d.Images.InlineImages)
	out, err := bridgeBPSToolAttachments(projected, d)
	require.NoError(t, err)
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.1.output.0.type").String())
	require.Equal(t, "https://example.invalid/image", gjson.GetBytes(out, "input.1.output.1.image_url").String())
	require.Equal(t, "file-existing", gjson.GetBytes(out, "input.3.content.1.file_id").String())
	require.Equal(t, "low", gjson.GetBytes(out, "input.3.content.1.detail").String())
	require.Equal(t, 1, d.Images.ToolAttachmentMessages)
	require.Equal(t, "message", d.Images.Details[0].OutboundItemType)
	require.Equal(t, "tool_attachment_message", d.Images.Details[0].Action)
	require.Equal(t, gjson.GetBytes(body, "input.1.output").Raw, gjson.GetBytes(out, "input.2.output").Raw)
	again, err := bridgeBPSToolAttachments(out, nil)
	require.NoError(t, err)
	require.Equal(t, out, again)
}

func TestBPSImageFileReferencesKeepAttachmentBridge(t *testing.T) {
	imageURL := "data:image/png;base64," + bpsTestPNG(t)
	for _, kind := range []string{"custom_tool_call_output", "function_call_output"} {
		for _, mixed := range []bool{false, true} {
			parts := []any{map[string]any{"type": "input_image", "file_id": "file-existing", "detail": "original"}}
			if mixed {
				parts = append(parts, map[string]any{"type": "input_image", "image_url": imageURL, "detail": "low"})
			}
			body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"type": kind, "call_id": "real-call", "output": parts}}})
			require.NoError(t, err)
			projected, d, err := prepareCodexBPSBody(body, "cache", false)
			require.NoError(t, err)
			out, err := bridgeBPSToolAttachments(projected, d)
			require.NoError(t, err)
			wantKind := kind
			if mixed {
				wantKind = "function_call_output"
				require.Equal(t, imageURL, gjson.GetBytes(out, "input.1.output.1.image_url").String())
				require.Equal(t, "low", gjson.GetBytes(out, "input.1.output.1.detail").String())
			}
			require.Equal(t, wantKind, gjson.GetBytes(out, "input.1.type").String())
			require.Equal(t, "real-call", gjson.GetBytes(out, "input.1.call_id").String())
			require.Equal(t, "file-existing", gjson.GetBytes(out, "input.2.content.1.file_id").String())
			require.Equal(t, "original", gjson.GetBytes(out, "input.2.content.1.detail").String())
			require.Equal(t, "message", d.Images.Details[0].OutboundItemType)
			require.Equal(t, 1, d.Images.ToolAttachmentMessages)
			require.Zero(t, d.Images.Uploaded)
		}
	}
}

func TestBPSCustomImageResultsInlineExecutor(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "inline.db"))
	require.NoError(t, err)
	defer db.Close()
	a := &auth.Account{DBID: 92102, AccountID: "inline-image-account", AccessToken: "account-token", CodexBPS: true}
	imageURL := "data:image/png;base64," + bpsTestPNG(t)
	var output []any
	for i := 0; i < 31; i++ {
		output = append(output, map[string]any{"type": "input_image", "image_url": imageURL, "detail": "original"})
	}
	tool := map[string]any{"type": "custom", "name": "read_fixture", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: \"READ\""}}
	call := map[string]any{"type": "custom_tool_call", "call_id": "real-call", "name": "read_fixture", "input": "READ\n"}
	business := `{"type":"input_image","image_url":"business-data"}`
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "tools": []any{tool}, "input": []any{
		call,
		map[string]any{"type": "custom_tool_call_output", "call_id": "real-call", "name": "read_fixture", "id": "output-id", "large_integer": json.Number("9007199254740993"), "output": output},
		map[string]any{"type": "custom_tool_call_output", "call_id": "text-call", "output": business},
	}})
	require.NoError(t, err)
	original := bytes.Clone(body)
	responses := 0
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		require.False(t, strings.HasSuffix(r.URL.Path, "/attachments"), "custom images must not upload")
		responses++
		data, e := io.ReadAll(r.Body)
		require.NoError(t, e)
		items := gjson.GetBytes(data, "input").Array()
		require.Len(t, items, 5) // runtime, tools, real call, image result, business string
		originalTool, _ := json.Marshal(tool)
		require.JSONEq(t, string(originalTool), items[1].Get("tools.0").Raw)
		originalCall, _ := json.Marshal(call)
		require.JSONEq(t, string(originalCall), items[2].Raw)
		require.Equal(t, "function_call_output", items[3].Get("type").String())
		require.Equal(t, "real-call", items[3].Get("call_id").String())
		require.Equal(t, "read_fixture", items[3].Get("name").String())
		require.Equal(t, "output-id", items[3].Get("id").String())
		require.Equal(t, "9007199254740993", items[3].Get("large_integer").Raw)
		parts := items[3].Get("output").Array()
		require.Len(t, parts, 31)
		for _, part := range parts {
			require.Equal(t, imageURL, part.Get("image_url").String())
			require.Equal(t, "original", part.Get("detail").String())
			require.False(t, part.Get("file_id").Exists())
		}
		require.Equal(t, "custom_tool_call_output", items[4].Get("type").String())
		require.Equal(t, business, items[4].Get("output").String())
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
	})
	for round := 0; round < 3; round++ {
		ctx := WithCodexAccountTestIdentityStore(t.Context(), db, a)
		var resp *http.Response
		if round == 2 {
			resp, err = ExecuteCompactRequest(ctx, a, body, "session", "", "", nil, nil)
		} else {
			resp, err = ExecuteRequest(ctx, a, body, "session", "", "", nil, nil, false)
		}
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		d := CodexBPSResponseDiagnostic(resp)
		require.Equal(t, 31, d.Images.Count)
		require.Equal(t, 31, d.Images.InlineImages)
		require.Equal(t, 1, d.Images.ToolOutputConversions)
		require.Zero(t, d.Images.Uploaded)
		require.Zero(t, d.Images.UploadReused)
		require.Zero(t, d.Images.ToolAttachmentMessages)
		require.Len(t, d.Images.Details, 8)
		require.Equal(t, 23, d.Images.DetailsOmitted)
		logged, e := json.Marshal(d)
		require.NoError(t, e)
		for _, value := range []string{imageURL, "real-call", "read_fixture", "business-data"} {
			require.NotContains(t, string(logged), value)
		}
		require.Equal(t, original, body)
	}
	require.Equal(t, 3, responses)
}

func TestBPSInvalidCustomImageStopsBeforeSending(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "invalid-inline.db"))
	require.NoError(t, err)
	defer db.Close()
	a := &auth.Account{DBID: 92103, AccountID: "invalid-inline-account", AccessToken: "token", CodexBPS: true}
	installClaudeBoundaryTransport(t, a, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid image must fail before any upload or inference")
		return nil, nil
	})
	pngData, err := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, err)
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(pngData, make([]byte, 1024)...))
	for _, scenario := range []struct{ image, callID string }{
		{"data:image/png;base64,!!!", "real-call"},
		{valid + "!", "real-call"},
		{valid, ""},
	} {
		body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"type": "custom_tool_call_output", "call_id": scenario.callID, "output": []any{map[string]any{"type": "input_image", "image_url": scenario.image}}}}})
		for _, compact := range []bool{false, true} {
			ctx := WithCodexAccountTestIdentityStore(t.Context(), db, a)
			if compact {
				_, err = ExecuteCompactRequest(ctx, a, body, "session", "", "", nil, nil)
			} else {
				_, err = ExecuteRequest(ctx, a, body, "session", "", "", nil, nil, false)
			}
			var inputErr *Error
			require.ErrorAs(t, err, &inputErr)
			require.Equal(t, 400, inputErr.HTTPStatus)
			require.Equal(t, "invalid_image_input", inputErr.Code)
		}
	}
}
