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
	"sync/atomic"
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
	require.Equal(t, "recent_attachments_v2", d.Policy)
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

func TestBPSAttachmentHistoryTenToFourBeforeUploadAndFallback(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, mode := range []string{"normal", "first_upload_429", "cooldown"} {
		t.Run(mode, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "trim-fallback.db"))
			require.NoError(t, err)
			defer db.Close()
			a := &auth.Account{DBID: 9988302, AccountID: NewUpstreamSessionUUID(), AccessToken: "test-only", CodexBPS: true, CodexBPSImageTrim: true}
			key := bpsAttachmentFallbackKey(a)
			t.Cleanup(func() { bpsFallbacks.mu.Lock(); delete(bpsFallbacks.entries, key); bpsFallbacks.mu.Unlock() })
			if mode == "cooldown" {
				(&bpsFallbackState{key: key, registry: bpsFallbacks}).trip(t.Context())
			}
			body := bpsAttachmentHistoryTestBody(t)
			var oldContent []json.RawMessage
			for _, part := range gjson.GetBytes(body, "input.0.content").Array() {
				oldContent = append(oldContent, json.RawMessage(part.Raw))
			}
			// These binary files would fail text conversion if accidentally restored
			// after trimming. Their source labels may remain, but never their bytes.
			for i := 0; i < 3; i++ {
				part, e := json.Marshal(map[string]string{"type": "input_file", "filename": fmt.Sprintf("old-%d.bin", i), "file_data": base64.StdEncoding.EncodeToString([]byte{0, 0xff, byte(i)})})
				require.NoError(t, e)
				oldContent = append(oldContent, part)
			}
			encoded, err := json.Marshal(oldContent)
			require.NoError(t, err)
			body, err = sjson.SetRawBytes(body, "input.0.content", encoded)
			require.NoError(t, err)
			var uploads atomic.Int32
			installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/attachments") {
					uploads.Add(1)
					status, data := 200, `{"openai_file_id":"file-retained-four"}`
					if mode != "normal" {
						status, data = 429, `{"error":{"type":"server_error","message":"429: Rate limit exceeded"}}`
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), Request: r}, nil
				}
				wire, e := io.ReadAll(r.Body)
				require.NoError(t, e)
				require.NotContains(t, string(wire), "file_data")
				require.NotContains(t, string(wire), "old-user-file")
				if mode != "normal" {
					for _, kept := range []string{"current-user-file", "result_2", "result_4"} {
						require.Contains(t, string(wire), kept)
					}
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
			})
			resp, err := ExecuteRequest(WithCodexAccountTestIdentityStore(t.Context(), db, a), a, body, testRootSessionA, "", "", nil, nil, false)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			d := CodexBPSResponseDiagnostic(resp)
			require.NotNil(t, d)
			require.NotNil(t, d.ImageHistory)
			require.Equal(t, 10, d.ImageHistory.AttachmentsBefore)
			require.Equal(t, 4, d.ImageHistory.AttachmentsAfter)
			require.Equal(t, 6, d.ImageHistory.AttachmentsOmitted)
			require.Equal(t, 3, d.Files.Count)
			require.Equal(t, 1, d.ImageHistory.After, "image scan count can include images omitted before preparation")
			require.Equal(t, 4, d.Timing.values.AttachmentDecodes, "only the four retained source attachments enter preparation")
			if mode == "normal" {
				require.EqualValues(t, 4, uploads.Load())
				require.Zero(t, d.Files.Fallback)
			} else {
				require.Equal(t, 3, d.Files.Fallback)
				require.Equal(t, 1, d.Images.InlineImages)
				require.Equal(t, 3, d.Timing.values.AttachmentFallbackFiles)
				require.Equal(t, 1, d.Timing.values.AttachmentFallbackImages)
				want := int32(0)
				if mode == "first_upload_429" {
					want = 1
				}
				require.Equal(t, want, uploads.Load())
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
			want := "input_file"
			if scenario == "missing_turn" || scenario == "unknown_item_turn" {
				want = "input_text"
			}
			require.Equal(t, want, gjson.GetBytes(out, "input.1.content.1.type").String())
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

func TestBPSAttachmentHistoryOrdinaryForkAndWorkerTurns(t *testing.T) {
	for _, tc := range []struct {
		name, source, kind, marker string
		headerOnly                 bool
	}{
		{"user_fork", "user", "", "", false},
		{"worker", "subagent", "thread_spawn", "collab_spawn", false},
		{"worker_header", "subagent", "thread_spawn", "collab_spawn", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bpsAttachmentHistoryTestBody(t)
			meta := map[string]any{"session_id": testRootSessionA, "thread_id": testLeafSessionA, "forked_from_thread_id": testRootSessionA, "request_kind": "turn", "thread_source": tc.source, "turn_id": bpsHistoryCurrentTurn}
			if tc.kind != "" {
				meta["subagent_kind"] = tc.kind
			}
			headers := make(http.Header)
			if tc.headerOnly {
				body, _ = sjson.DeleteBytes(body, "client_metadata")
				raw, err := json.Marshal(meta)
				require.NoError(t, err)
				headers.Set(codexTurnMetadataHeader, string(raw))
				headers.Set("X-OpenAI-Subagent", tc.marker)
			} else {
				body, _ = sjson.SetBytes(body, "client_metadata", map[string]any{"session_id": testRootSessionA, "thread_id": testLeafSessionA, "x-codex-turn-metadata": meta, "x-openai-subagent": tc.marker})
			}
			root := resolveRequestRootSessionIdentity(headers, body)
			require.True(t, root.related, "test must exercise related-session history")
			require.False(t, root.conflict)
			out, d, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, headers)
			require.NoError(t, err)
			require.Empty(t, d.ImageHistory.SkipReason)
			require.Equal(t, 7, d.ImageHistory.AttachmentsBefore)
			require.Equal(t, 4, d.ImageHistory.AttachmentsAfter)
			require.Equal(t, 4, bpsCountHistoryImages(out)+bpsHistoryCountFiles(out))
		})
	}
}

func TestBPSAttachmentHistoryWithoutTurnLabels(t *testing.T) {
	body := bpsAttachmentHistoryTestBody(t)
	body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-metadata.turn_id")
	for i := range gjson.GetBytes(body, "input").Array() {
		body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough.turn_id", i))
	}
	original := bytes.Clone(body)
	out, d, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, nil)
	require.NoError(t, err)
	require.Empty(t, d.ImageHistory.SkipReason)
	require.Equal(t, "history_order", d.ImageHistory.BoundarySource)
	require.Equal(t, 4, d.ImageHistory.AttachmentsAfter)
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.1.content.1.type").String())
	require.Equal(t, "input_file", gjson.GetBytes(out, "input.3.content.0.type").String())
	require.Equal(t, 1, d.ImageHistory.RetainedReasons["latest_user_segment"])
	require.Equal(t, 2, d.ImageHistory.RetainedReasons["recent_tool"])
	require.Equal(t, 1, d.ImageHistory.RetainedReasons["latest_tool_group"])
	require.Equal(t, original, body)
	again, _, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(out), string(again))
}

func TestBPSAttachmentHistoryPreservesUnseparatedUserFragments(t *testing.T) {
	// Consecutive user fragments may belong to one current prompt; neither a
	// missing turn ID nor an intervening system instruction makes them old.
	items := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"input_file","file_id":"file-current-one"}]}`),
		json.RawMessage(`{"role":"system","content":"task context"}`),
		json.RawMessage(`{"role":"user","content":[{"type":"input_image","file_id":"file-current-two"}]}`),
		json.RawMessage(`{"role":"assistant","content":"Read both."}`),
	}
	d := &CodexBPSDiagnostic{}
	out, err := trimBPSImageHistory(items, []byte(`{"model":"gpt-6-astra"}`), nil, false, d)
	require.NoError(t, err)
	require.Equal(t, items, out)
	require.Equal(t, 2, d.ImageHistory.RetainedReasons["latest_user_segment"])
}

func TestBPSAttachmentHistorySkippedRequestsStillCountAttachments(t *testing.T) {
	for _, tc := range []struct{ source, kind, marker string }{
		{"guardian_review", "", ""},
		{"subagent", "guardian_classifier", "guardian_classifier"},
		{"subagent", "", ""},
		{"user", "", "memory_consolidation"},
	} {
		t.Run(tc.source+"/"+tc.kind+"/"+tc.marker, func(t *testing.T) {
			body := bpsAttachmentHistoryTestBody(t)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.thread_source", tc.source)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.subagent_kind", tc.kind)
			if tc.marker != "" {
				// Flat header marker applies when the canonical kind is absent.
				body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-metadata.subagent_kind")
				body, _ = sjson.SetBytes(body, "client_metadata.x-openai-subagent", tc.marker)
			}
			_, d, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, nil)
			require.NoError(t, err)
			require.Equal(t, "non_user_request", d.ImageHistory.SkipReason)
			require.Equal(t, 7, d.ImageHistory.AttachmentsBefore)
			require.Equal(t, 7, d.ImageHistory.AttachmentsAfter)
			require.Equal(t, 7, d.ImageHistory.RetainedReasons["policy_skipped"])
		})
	}
}

func TestBPSAttachmentHistoryUnknownRelatedRequestIsConservative(t *testing.T) {
	body := bpsAttachmentHistoryTestBody(t)
	body, _ = sjson.DeleteBytes(body, "client_metadata")
	headers := nativeSessionHeaders(testRootSessionA, testLeafSessionA, 1)
	require.True(t, resolveRequestRootSessionIdentity(headers, body).related)
	out, d, err := prepareCodexBPSBodyWithImageTrim(body, "history", false, true, headers)
	require.NoError(t, err)
	require.Equal(t, "non_user_request", d.ImageHistory.SkipReason)
	require.Equal(t, 7, bpsCountHistoryImages(out)+bpsHistoryCountFiles(out))
}
