package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func bpsAttachmentHistoryTestBody(t *testing.T) []byte {
	t.Helper()
	body := bpsHistoryTestBody(t)
	setFile := func(path, name, data string) {
		part, err := json.Marshal(map[string]any{"type": "input_file", "filename": name, "file_data": base64.StdEncoding.EncodeToString([]byte(data)), "opaque_number": json.Number("9007199254740993")})
		require.NoError(t, err)
		body, err = sjson.SetRawBytes(body, path, part)
		require.NoError(t, err)
	}
	setFile("input.0.content.1", `C:\private\old.pdf`, "old-user-file")
	setFile("input.2.content.0", "current.txt", "current-user-file")
	for i, item := range gjson.GetBytes(body, "input").Array() {
		switch item.Get("id").String() {
		case "result_0", "result_2", "result_4":
			setFile(fmt.Sprintf("input.%d.output.1", i), item.Get("id").String()+".txt", item.Get("id").String())
		}
	}
	return body
}

func bpsHistoryCountFiles(body []byte) int {
	count := 0
	for _, item := range gjson.GetBytes(body, "input").Array() {
		for _, field := range []string{"content", "output"} {
			for _, part := range item.Get(field).Array() {
				if part.Get("type").String() == "input_file" {
					count++
				}
			}
		}
	}
	return count
}

func TestBPSAttachmentHistorySharesThreeRecentSlots(t *testing.T) {
	body := bpsAttachmentHistoryTestBody(t)
	original := bytes.Clone(body)
	full, off, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, false, nil)
	require.NoError(t, err)
	require.Nil(t, off.ImageHistory)
	require.Equal(t, 5, bpsHistoryCountFiles(full))
	require.Equal(t, 2, bpsCountHistoryImages(full))
	out, diagnostic, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, nil)
	require.NoError(t, err)
	d := diagnostic.ImageHistory
	require.Equal(t, "recent_attachments_v1", d.Policy)
	require.Equal(t, 3, d.RecentToolAttachments)
	require.Equal(t, 7, d.AttachmentsBefore)
	require.Equal(t, 4, d.AttachmentsAfter)
	require.Equal(t, 3, d.AttachmentsOmitted)
	require.Equal(t, 5, d.FilesBefore)
	require.Equal(t, 3, d.FilesAfter)
	require.Equal(t, 2, d.FilesOmitted)
	require.Equal(t, 2, d.Before)
	require.Equal(t, 1, d.After)
	require.Equal(t, 1, d.Omitted)
	require.Equal(t, 3, bpsHistoryCountFiles(out))
	require.Equal(t, 1, bpsCountHistoryImages(out))
	note := gjson.GetBytes(out, "input.1.content.1.text").String()
	require.Contains(t, note, bpsOmittedFileNote)
	require.Contains(t, note, `old.pdf`)
	require.NotContains(t, string(out), base64.StdEncoding.EncodeToString([]byte("old-user-file")))
	require.Equal(t, "unknown", gjson.GetBytes(out, "input.1.internal_chat_message_metadata_passthrough.content_item_kinds.1").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "input.3.content.0.opaque_number").Raw)
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if strings.HasPrefix(item.Get("id").String(), "result_") {
			require.Equal(t, "custom_tool_call_output", item.Get("type").String())
			require.NotEmpty(t, item.Get("call_id").String())
			require.Equal(t, "Tool text stays.", item.Get("output.0.text").String())
		}
	}
	again, _, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(out), string(again))
	require.Equal(t, original, body)
	logged, err := json.Marshal(d)
	require.NoError(t, err)
	for _, secret := range []string{"private", "old.pdf", "old-user-file", "result_0.txt"} {
		require.NotContains(t, string(logged), secret)
	}
}

func TestBPSAttachmentHistoryExecutorUploadsOnlyKeptContent(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "attachment-history.db"))
			require.NoError(t, err)
			defer db.Close()
			a := &auth.Account{DBID: 9988301, AccountID: NewUpstreamSessionUUID(), AccessToken: "test-only", CodexBPS: true, CodexBPSImageTrim: enabled}
			var mu sync.Mutex
			contents := map[string]int{}
			installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/attachments") {
					reader, err := r.MultipartReader()
					require.NoError(t, err)
					part, err := reader.NextPart()
					require.NoError(t, err)
					data, err := io.ReadAll(part)
					require.NoError(t, err)
					mu.Lock()
					contents[string(data)]++
					mu.Unlock()
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-retained"}`)), Request: r}, nil
				}
				wire, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NotContains(t, string(wire), "file_data")
				if enabled {
					require.Contains(t, string(wire), bpsOmittedFileNote)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
			})
			body := bpsAttachmentHistoryTestBody(t)
			resp, err := ExecuteRequest(WithCodexAccountTestIdentityStore(t.Context(), db, a), a, body, testRootSessionA, "", "", nil, nil, false)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			for _, kept := range []string{"current-user-file", "result_2", "result_4"} {
				require.Equal(t, 1, contents[kept])
			}
			for _, omitted := range []string{"old-user-file", "result_0"} {
				if enabled {
					require.Zero(t, contents[omitted])
				} else {
					require.Equal(t, 1, contents[omitted])
				}
			}
			parts := bpsUploadParts(body, nil, false, enabled)
			if enabled {
				require.Len(t, parts, 4)
			} else {
				require.Len(t, parts, 7)
			}
		})
	}
}

func TestBPSAttachmentHistoryKeepsLatestGroupAndConservativeCases(t *testing.T) {
	body := bpsAttachmentHistoryTestBody(t)
	last := len(gjson.GetBytes(body, "input").Array()) - 1
	part := gjson.GetBytes(body, fmt.Sprintf("input.%d.output.1", last)).Raw
	body, err := sjson.SetRawBytes(body, fmt.Sprintf("input.%d.output", last), []byte("["+strings.TrimSuffix(strings.Repeat(part+",", 6), ",")+"]"))
	require.NoError(t, err)
	_, d, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, nil)
	require.NoError(t, err)
	require.Equal(t, 7, d.ImageHistory.FilesAfter)
	for _, scenario := range []string{"compact", "missing_turn", "passive", "unknown_item_turn", "unknown_file_shape"} {
		t.Run(scenario, func(t *testing.T) {
			body := bpsAttachmentHistoryTestBody(t)
			switch scenario {
			case "missing_turn":
				body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-metadata.turn_id")
			case "passive":
				body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.thread_source", "guardian_review")
			case "unknown_item_turn":
				body, _ = sjson.DeleteBytes(body, "input.0.internal_chat_message_metadata_passthrough.turn_id")
			case "unknown_file_shape":
				body, _ = sjson.SetBytes(body, "input.0.content.1.file_data", true)
			}
			out, _, err := prepareCodexBPSBodyWithImageTrim(body, "history", scenario == "compact", true, nil)
			require.NoError(t, err)
			require.Equal(t, "input_file", gjson.GetBytes(out, "input.1.content.1.type").String())
		})
	}
}

func TestBPSAttachmentHistorySourceNotesNeverInventOrEmbedContent(t *testing.T) {
	part := gjson.Parse(`{"type":"input_file","filename":"report.pdf","file_url":"https://example.invalid/original.pdf","file_id":"file-old","file_data":"PRIVATE_BYTES"}`)
	out := bpsOmittedAttachmentPart(part)
	note := gjson.GetBytes(out, "text").String()
	for _, source := range []string{"report.pdf", "https://example.invalid/original.pdf", "file-old"} {
		require.Contains(t, note, source)
	}
	require.NotContains(t, note, "PRIVATE_BYTES")
	part = gjson.Parse(`{"type":"input_file","file_data":"PRIVATE_BYTES","file_url":"data:application/pdf;base64,PRIVATE_BYTES"}`)
	out = bpsOmittedAttachmentPart(part)
	require.NotContains(t, string(out), "PRIVATE_BYTES")
	require.NotContains(t, string(out), "data:")
	require.Equal(t, bpsOmittedFileNote, gjson.GetBytes(out, "text").String())
	// Audio/video, tool declarations, call arguments and embedded JSON remain opaque.
	raw := json.RawMessage(`{"type":"function_call_output","call_id":"opaque","output":[{"type":"input_text","text":"{\"type\":\"input_file\",\"file_data\":\"keep\"}"},{"type":"input_audio","data":"keep"},{"type":"input_video","video_url":"keep"}],"internal_chat_message_metadata_passthrough":{"turn_id":"old"}}`)
	items := []json.RawMessage{raw, json.RawMessage(`{"role":"assistant","content":"done"}`)}
	trimmed, err := trimBPSImageHistory(items, bpsAttachmentHistoryTestBody(t), nil, false, &CodexBPSDiagnostic{})
	require.NoError(t, err)
	require.Equal(t, raw, trimmed[0])
}
