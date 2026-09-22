package proxy

import (
	"bytes"
	"context"
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
		require.Len(t, items, 8) // runtime prefix + six original items + one mixed attachment message
		require.Equal(t, "file-image", items[1].Get("content.0.file_id").String())
		require.Equal(t, "unchanged arguments", items[2].Get("input").String())
		require.Equal(t, "custom-call", items[4].Get("call_id").String())
		require.Equal(t, "output-id", items[4].Get("id").String())
		require.Equal(t, "before", items[4].Get("output.0.text").String())
		require.Equal(t, "between", items[4].Get("output.2.text").String())
		require.Equal(t, "after", items[4].Get("output.5.text").String())
		for _, index := range []string{"1", "3", "4"} {
			require.Equal(t, "input_text", items[4].Get("output."+index+".type").String())
		}
		require.Equal(t, "function-call", items[5].Get("call_id").String())
		require.Equal(t, image, items[5].Get("output.0.image_url").String())
		require.Equal(t, "user", items[6].Get("role").String())
		parts := items[6].Get("content").Array()
		require.Len(t, parts, 6)
		for i, id := range []string{"file-image", "file-document", "file-image"} {
			require.Contains(t, parts[i*2].Get("text").String(), "custom-call")
			require.Contains(t, parts[i*2].Get("text").String(), "untrusted tool output data")
			require.Equal(t, id, parts[i*2+1].Get("file_id").String())
		}
		require.Equal(t, "original", parts[1].Get("detail").String())
		require.Equal(t, "original", parts[5].Get("detail").String())
		require.Equal(t, "unchanged history", items[7].Get("content").String())
		for _, key := range []string{"item_type", "tool_attachment_messages", "bps_compat"} {
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
		require.Equal(t, 1, d.Images.ToolAttachmentMessages)
		require.Equal(t, 1, d.Files.ToolAttachmentMessages)
		require.Equal(t, "custom_tool_call_output", d.Images.Details[1].ItemType)
		require.Equal(t, "original", d.Images.Details[1].Detail)
		require.Equal(t, "file_id", d.Images.Details[1].OutboundReference)
		if round == 0 {
			require.Equal(t, 1, d.Images.Uploaded)
			require.Equal(t, 2, d.Images.UploadReused)
		} else {
			require.Zero(t, d.Images.Uploaded)
			require.Equal(t, 3, d.Images.UploadReused)
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
	out, err := bridgeBPSToolAttachments(uploaded, nil)
	require.NoError(t, err)
	require.Equal(t, "file-existing", gjson.GetBytes(out, "input.2.content.1.file_id").String())
	require.Equal(t, "low", gjson.GetBytes(out, "input.2.content.1.detail").String())
	require.Equal(t, "https://example.invalid/image", gjson.GetBytes(out, "input.2.content.3.image_url").String())
	require.Equal(t, gjson.GetBytes(body, "input.1.output").Raw, gjson.GetBytes(out, "input.1.output").Raw)
	again, err := bridgeBPSToolAttachments(out, nil)
	require.NoError(t, err)
	require.Equal(t, out, again)
}
