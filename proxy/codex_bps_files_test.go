package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSFileDataDecodingWithoutFormatAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, value, filename, wantName, wantMIME string
		want                                      []byte
	}{
		{"unknown", "data:application/vnd.future.example;base64,AP8B", "report.future-format", "report.future-format", "application/vnd.future.example", []byte{0, 255, 1}},
		{"raw", "aGVsbG8=", "report.txt", "report.txt", "text/plain", []byte("hello")},
		{"unpadded", "aGVsbG8", "report.txt", "report.txt", "text/plain", []byte("hello")},
		{"percent", "data:application/x-future,a%2Bb+c%00", `C:\private\report.newtype`, "report.newtype", "application/x-future", []byte("a+b+c\x00")},
		{"uppercase", "DATA:application/pdf;BASE64,aGVsbG8=", "/home/private/report.pdf", "report.pdf", "application/pdf", []byte("hello")},
		{"empty", "", "empty.txt", "empty.txt", "text/plain", []byte{}},
		{"quoted", "data:application/octet-stream;base64,aGVsbG8=", "引号\"文件\r\n.txt", "引号\"文件__.txt", "application/octet-stream", []byte("hello")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := decodeBPSFileData(tc.value, tc.filename)
			require.NoError(t, err)
			require.Equal(t, tc.want, file.Data)
			require.Equal(t, tc.wantName, file.Name)
			require.True(t, strings.HasPrefix(file.ContentType, tc.wantMIME))
		})
	}
	for _, value := range []string{"data:application/pdf;base64", "data:application/pdf;base64,!!!", "data:bad media type;base64,YQ==", "data:text/plain,%GG", "not base64 !"} {
		_, err := decodeBPSFileData(value, "private-name.secret")
		var e *Error
		require.ErrorAs(t, err, &e)
		require.Equal(t, 400, e.HTTPStatus)
		require.NotContains(t, e.Error(), "private-name")
	}
	a := &auth.Account{DBID: 90101, AccountID: "account-a"}
	b := &auth.Account{DBID: 90102, AccountID: "account-b"}
	f := bpsFileAttachment{Data: []byte("same"), Name: "one.txt", ContentType: "text/plain"}
	key := bpsFileUploadKey(a, f)
	require.NotEqual(t, key, bpsFileUploadKey(b, f))
	f.Name = "two.txt"
	require.NotEqual(t, key, bpsFileUploadKey(a, f))
	f.Name = "one.txt"
	f.ContentType = "application/custom"
	require.NotEqual(t, key, bpsFileUploadKey(a, f))
}

func TestBPSFilesExecutorPreservesBytesCarriersAndAccountScope(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "files.db"))
	require.NoError(t, err)
	defer db.Close()
	for _, accountID := range []int64{90201, 90202} {
		a := withBPSOverride(&auth.Account{DBID: accountID, AccountID: fmt.Sprint(accountID), AccessToken: "selected-token"}, true)
		uploads, responses := 0, 0
		fileID := fmt.Sprintf("file-account-%d", accountID)
		installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "Bearer selected-token", r.Header.Get("Authorization"))
			require.Equal(t, a.AccountID, r.Header.Get("Chatgpt-Account-Id"))
			require.Empty(t, r.Header.Get(codexTurnStateHeader))
			if strings.HasSuffix(r.URL.Path, "/attachments") {
				uploads++
				mr, e := r.MultipartReader()
				require.NoError(t, e)
				p, e := mr.NextPart()
				require.NoError(t, e)
				require.Equal(t, "future.unknown-extension", p.FileName())
				require.Equal(t, "application/vnd.future.example", p.Header.Get("Content-Type"))
				data, e := io.ReadAll(p)
				require.NoError(t, e)
				require.Equal(t, []byte{0, 255, 1}, data)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"` + fileID + `"}`)), Request: r}, nil
			}
			responses++
			data, e := io.ReadAll(r.Body)
			require.NoError(t, e)
			for i, field := range []string{"content", "output", "output"} {
				root := fmt.Sprintf("input.%d", i+1)
				p := gjson.GetBytes(data, root+"."+field+".1")
				if i > 0 {
					require.Equal(t, "input_text", p.Get("type").String())
					require.Contains(t, p.Get("text").String(), "tool output position (2)")
					p = gjson.GetBytes(data, fmt.Sprintf("input.4.content.%d", 2*(i-1)+1))
					require.Contains(t, gjson.GetBytes(data, fmt.Sprintf("input.4.content.%d.text", 2*(i-1))).String(), fmt.Sprintf("call-%d", i))
				}
				require.Equal(t, fileID, p.Get("file_id").String())
				require.False(t, p.Get("file_data").Exists())
				require.False(t, p.Get("filename").Exists())
				require.False(t, p.Get("file_url").Exists())
				require.Equal(t, "9007199254740993", p.Get("opaque_number").Raw)
				require.Equal(t, "high", p.Get("detail").String())
				require.Equal(t, "before", gjson.GetBytes(data, root+"."+field+".0.text").String())
				require.Equal(t, "after", gjson.GetBytes(data, root+"."+field+".2.text").String())
				if i > 0 {
					require.Equal(t, fmt.Sprintf("call-%d", i), gjson.GetBytes(data, root+".call_id").String())
				}
			}
			require.Len(t, gjson.GetBytes(data, "input").Array(), 5)
			require.Equal(t, "user", gjson.GetBytes(data, "input.4.role").String())
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
		})
		part := `{"type":"input_file","filename":"future.unknown-extension","file_data":"data:application/vnd.future.example;base64,AP8B","file_id":"stale-id","file_url":"https://unused.invalid/a","detail":"high","opaque_number":9007199254740993}`
		content := `[{"type":"input_text","text":"before"},` + part + `,{"type":"input_text","text":"after"}]`
		body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":` + content + `},{"type":"function_call_output","call_id":"call-1","output":` + content + `},{"type":"custom_tool_call_output","call_id":"call-2","output":` + content + `}]}`)
		original := bytes.Clone(body)
		for round := 0; round < 3; round++ {
			ctx := WithCodexAccountTestIdentityStore(t.Context(), db, a)
			var resp *http.Response
			if round == 2 {
				resp, err = executeBPSTestCompact(ctx, a, body, "session", "", "", nil, nil)
			} else {
				resp, err = executeBPSTestRequest(ctx, a, body, "session", "", "", nil, nil, false)
			}
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			d := CodexBPSResponseDiagnostic(resp)
			require.NotNil(t, d.Files)
			require.Equal(t, 3, d.Files.Count)
			require.Equal(t, 1, d.Files.ToolAttachmentMessages)
			if round == 0 {
				require.Equal(t, 1, d.Files.Uploaded)
				require.Equal(t, 2, d.Files.UploadReused)
			} else {
				require.Zero(t, d.Files.Uploaded)
				require.Equal(t, 3, d.Files.UploadReused)
			}
			logged, e := json.Marshal(d)
			require.NoError(t, e)
			for _, private := range []string{"AP8B", fileID, "future.unknown-extension", "application/vnd.future.example"} {
				require.NotContains(t, string(logged), private)
			}
			require.Equal(t, original, body)
		}
		require.Equal(t, 1, uploads)
		require.Equal(t, 3, responses)
	}
}

func TestBPSFilesOnlyRewriteProtocolFileData(t *testing.T) {
	a := &auth.Account{DBID: 90301, AccountID: "business-fixture"}
	part := map[string]any{"type": "input_file", "filename": "private.txt", "file_data": base64.StdEncoding.EncodeToString([]byte("business"))}
	encoded, err := json.Marshal(part)
	require.NoError(t, err)
	for _, item := range []any{
		map[string]any{"type": "function_call", "arguments": string(encoded)},
		map[string]any{"type": "function_call_output", "output": string(encoded)},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": string(encoded)}}},
		map[string]any{"type": "additional_tools", "tools": []any{part}},
		map[string]any{"type": "reasoning", "encrypted_content": string(encoded)},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_file", "file_id": "existing-id"}, map[string]any{"type": "input_file", "file_url": "https://private.invalid/file"}}},
	} {
		body, e := json.Marshal(map[string]any{"input": []any{item}})
		require.NoError(t, e)
		out, used, e := prepareBPSFileAttachments(t.Context(), a, body, nil, func(context.Context, bpsFileAttachment) (string, error) { t.Fatal("unexpected upload"); return "", nil })
		require.NoError(t, e)
		require.Empty(t, used)
		require.Equal(t, body, out)
	}
	items := []any{}
	for i := 0; i < 20; i++ {
		items = append(items, map[string]any{"role": "user", "content": []any{part}})
	}
	body, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)
	d := &CodexBPSDiagnostic{}
	_, _, err = prepareBPSFileAttachments(t.Context(), a, body, d, func(context.Context, bpsFileAttachment) (string, error) { return "file-summary-test", nil })
	require.NoError(t, err)
	require.Equal(t, 20, d.Files.Count)
	require.Len(t, d.Files.Details, 8)
	require.Equal(t, 12, d.Files.DetailsOmitted)
	require.Equal(t, "input[19].content[0]", d.Files.Details[7].Path)
}
