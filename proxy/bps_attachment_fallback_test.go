package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func fallbackTestContext(t *testing.T, key string) (context.Context, *bpsFallbackState) {
	t.Helper()
	s := &bpsFallbackState{key: key, registry: &bpsFallbackRegistry{entries: map[string]*bpsFallbackEntry{}}}
	return context.WithValue(t.Context(), bpsFallbackContextKey{}, s), s
}

func TestBPSAttachmentFallbackWindowRecoveryAndCache(t *testing.T) {
	ctx, s := fallbackTestContext(t, "account")
	local := newBPSLocalCache()
	calls := 0
	reject := true
	upload := func(c context.Context) (string, error) {
		return bpsUploadWithFallback(c, func(context.Context) (string, error) {
			calls++
			if reject {
				return "", uploadCooldownTestError(t, 429, "1", `{"error":{"type":"server_error","message":"429: Rate limit exceeded"}}`)
			}
			return "file-recovered", nil
		})
	}
	_, _, err := local.resolveContext(ctx, "first", upload)
	require.ErrorIs(t, err, errBPSAttachmentFallback)
	require.WithinDuration(t, time.Now().Add(2*time.Hour), s.registry.entries[s.key].until, 2*time.Second)
	require.Empty(t, local.entries, "fallback is never cached as a successful handle")
	_, _, err = local.resolveContext(ctx, "second", upload)
	require.ErrorIs(t, err, errBPSAttachmentFallback)
	require.Equal(t, 1, calls)
	// A previous valid handle remains usable during the entire cooldown.
	_, _, err = local.resolveContext(t.Context(), "cached", func(context.Context) (string, error) { return "file-existing", nil })
	require.NoError(t, err)
	id, reused, err := local.resolveContext(ctx, "cached", upload)
	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, "file-existing", id)
	require.Equal(t, 1, calls)
	// Expiry followed by another 429 starts a fresh two-hour cycle.
	s.registry.entries[s.key].until = time.Now().Add(-time.Second)
	_, _, err = local.resolveContext(ctx, "second", upload)
	require.ErrorIs(t, err, errBPSAttachmentFallback)
	require.Equal(t, 2, calls)
	require.WithinDuration(t, time.Now().Add(2*time.Hour), s.registry.entries[s.key].until, 2*time.Second)
	// Recovery success restores normal upload behavior and produces a real ID.
	s.registry.entries[s.key].until = time.Now().Add(-time.Second)
	reject = false
	id, _, err = local.resolveContext(ctx, "second", upload)
	require.NoError(t, err)
	require.Equal(t, "file-recovered", id)
	require.Empty(t, s.registry.entries)
	_, _, err = local.resolveContext(ctx, "third", upload)
	require.NoError(t, err)
	require.Equal(t, 4, calls)
}

func TestBPSAttachmentFallbackOneRecoveryProbe(t *testing.T) {
	ctx, s := fallbackTestContext(t, "probe")
	s.remember(time.Now().Add(-time.Second))
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := bpsUploadWithFallback(ctx, func(context.Context) (string, error) { close(started); <-finish; return "file-probe", nil })
		done <- err
	}()
	<-started
	var wg sync.WaitGroup
	var attempted atomic.Int32
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := bpsUploadWithFallback(ctx, func(context.Context) (string, error) { attempted.Add(1); return "file-bad", nil })
			if err != errBPSAttachmentFallback {
				t.Errorf("expected fallback during probe: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Zero(t, attempted.Load())
	close(finish)
	require.NoError(t, <-done)
}

func TestBPSAttachmentFallbackSharedRestartAndAccountIsolation(t *testing.T) {
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	defer backend.Close()
	a := &auth.Account{DBID: 33001, AccountID: "same-workspace"}
	b := &auth.Account{DBID: 33002, AccountID: "same-workspace"}
	require.NotEqual(t, bpsAttachmentFallbackKey(a), bpsAttachmentFallbackKey(b))
	ctx, s := fallbackTestContext(t, bpsAttachmentFallbackKey(a))
	s.backend = &bpsAttachmentBackend{store: backend}
	s.trip(ctx)
	ctx2, other := fallbackTestContext(t, bpsAttachmentFallbackKey(a))
	other.backend = &bpsAttachmentBackend{store: backend}
	require.True(t, other.active(ctx2), "a fresh process reloads the shared two-hour window")
	_, isolated := fallbackTestContext(t, bpsAttachmentFallbackKey(b))
	isolated.backend = &bpsAttachmentBackend{store: backend}
	require.False(t, isolated.active(t.Context()), "same workspace does not merge account records")
	// A stale success must not delete a newer 429 record.
	old := time.Now().Add(-time.Second)
	other.registry.entries[other.key].until = old
	other.backend = nil
	_, err := bpsUploadWithFallback(ctx2, func(c context.Context) (string, error) { other.trip(c); return "file-racing-success", nil })
	require.NoError(t, err)
	require.True(t, other.active(ctx2))
}

func TestBPSAttachmentFallbackOnlyUploadRateLimits(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{400, ""}, {401, ""}, {403, ""}, {500, ""}, {429, ""},
		{429, `{"error":{"code":"cyber_policy","message":"blocked"}}`},
		{429, `{"error":{"code":"insufficient_quota","message":"Rate limit exceeded"}}`},
		{429, `{"error":{"code":"rate_limit_exceeded","message":"Tokens per minute limit exceeded"}}`},
		{429, `{"error":{"message":"Server overloaded"}}`},
		{429, `{"error":{"message":"Account suspended"}}`},
		{429, `{"error":{"type":"insufficient_quota","message":"Rate limit exceeded"}}`},
	} {
		ctx, s := fallbackTestContext(t, "hard-stop")
		original := uploadCooldownTestError(t, tc.status, "", tc.body)
		_, err := bpsUploadWithFallback(ctx, func(context.Context) (string, error) { return "", original })
		require.ErrorIs(t, err, original)
		require.Empty(t, s.registry.entries)
	}
	ctx, s := fallbackTestContext(t, "inference-429")
	original := ErrUpstream(429, "Rate limit exceeded", nil)
	_, err := bpsUploadWithFallback(ctx, func(context.Context) (string, error) { return "", original })
	require.ErrorIs(t, err, original)
	require.Empty(t, s.registry.entries)
}

func TestBPSAttachmentFallbackSharedRecoveryProbe(t *testing.T) {
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	defer backend.Close()
	ctx, a := fallbackTestContext(t, "shared-probe")
	ctx2, b := fallbackTestContext(t, "shared-probe")
	a.backend = &bpsAttachmentBackend{store: backend}
	b.backend = &bpsAttachmentBackend{store: backend}
	raw, _ := json.Marshal(time.Now().Add(-time.Second))
	require.NoError(t, backend.SetRuntime(ctx, bpsAttachmentFallbackNamespace, a.key, raw, time.Minute))
	start, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := bpsUploadWithFallback(ctx, func(context.Context) (string, error) { close(start); <-finish; return "file-recovered", nil })
		done <- err
	}()
	<-start
	_, err := bpsUploadWithFallback(ctx2, func(context.Context) (string, error) {
		t.Error("second instance must not upload during recovery probe")
		return "file-invalid", nil
	})
	require.ErrorIs(t, err, errBPSAttachmentFallback)
	close(finish)
	require.NoError(t, <-done)
	_, found, err := backend.GetRuntime(ctx, bpsAttachmentFallbackNamespace, a.key)
	require.NoError(t, err)
	require.False(t, found)
}

func TestBPSAttachmentFallbackPDFAndLegacyConverterBoundaries(t *testing.T) {
	png, e := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, e)
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "pdf", true: "doc"}[legacy], func(t *testing.T) {
			var dir string
			rendered := 0
			run := bpsConverterRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				require.NotNil(t, ctx.Done(), "conversion has a deadline")
				switch name {
				case "libreoffice":
					input := args[len(args)-1]
					dir = filepath.Dir(input)
					profile, e := os.ReadFile(filepath.Join(dir, "profile", "user", "registrymodifications.xcu"))
					require.NoError(t, e)
					require.Contains(t, string(profile), `oor:name="DisableMacrosExecution"`)
					require.Contains(t, string(profile), `<value>true</value>`)
					require.NoError(t, os.WriteFile(filepath.Join(dir, "attachment.pdf"), []byte("%PDF-fixture"), 0600))
					return nil, nil
				case "pdfinfo":
					dir = filepath.Dir(args[0])
					return []byte("Pages: 2\n"), nil
				case "pdftotext":
					return []byte("All extracted text\fpage 2 text"), nil
				case "pdftoppm":
					rendered++
					require.NoError(t, os.WriteFile(args[len(args)-1]+".png", png, 0600))
					return nil, nil
				default:
					t.Fatalf("unexpected converter %q", name)
					return nil, nil
				}
			})
			ctx := context.WithValue(t.Context(), bpsConverterRunnerKey{}, run)
			file := bpsFileAttachment{Name: "sample.pdf", Data: []byte("%PDF-fixture")}
			if legacy {
				file = bpsFileAttachment{Name: "sample.doc", Data: []byte{0xd0, 0xcf, 0x11, 0xe0}}
			}
			parts, _, err := bpsFallbackDocument(ctx, file)
			require.NoError(t, err)
			require.Equal(t, 2, rendered)
			require.Len(t, parts, 5)
			require.Contains(t, gjson.GetBytes(parts[0], "text").String(), "page 2 text")
			_, err = os.Stat(dir)
			require.True(t, os.IsNotExist(err), "private temporary files are removed")
		})
	}
	ctx := context.WithValue(t.Context(), bpsConverterRunnerKey{}, bpsConverterRunner(func(_ context.Context, name string, _ ...string) ([]byte, error) {
		require.Equal(t, "pdfinfo", name)
		return []byte("Pages: 33\n"), nil
	}))
	parts, _, err := bpsFallbackPDF(ctx, []byte("%PDF-fixture"))
	require.Error(t, err)
	require.Nil(t, parts, "no first-32-pages partial success")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = bpsFallbackDocument(ctx, bpsFileAttachment{Name: "a.doc", Data: []byte{0xd0, 0xcf, 0x11, 0xe0}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestBPSAttachmentFallbackPreservesSuppliedURLInToolResult(t *testing.T) {
	ctx, s := fallbackTestContext(t, "url-test")
	s.trip(ctx)
	a := &auth.Account{DBID: 938219, AccountID: "url-test"}
	body := []byte(`{"input":[{"type":"custom_tool_call_output","call_id":"real","output":[{"type":"input_file","filename":"broken.pdf","file_data":"bm90LWEtdmFsaWQtcGRm","file_url":"https://example.invalid/original.pdf"}]}]}`)
	d := &CodexBPSDiagnostic{}
	out, _, err := prepareBPSFileAttachments(ctx, a, body, d, func(context.Context, bpsFileAttachment) (string, error) {
		t.Error("cooldown must not upload")
		return "", nil
	})
	require.NoError(t, err)
	out, err = bridgeBPSToolAttachments(out, d)
	require.NoError(t, err)
	require.Equal(t, "custom_tool_call_output", gjson.GetBytes(out, "input.0.type").String())
	require.Equal(t, "real", gjson.GetBytes(out, "input.0.call_id").String())
	require.Equal(t, "https://example.invalid/original.pdf", gjson.GetBytes(out, "input.1.content.1.file_url").String())
	require.NotContains(t, string(out), "file_data")
}

func fallbackZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, b := range files {
		f, e := w.Create(name)
		require.NoError(t, e)
		_, e = f.Write(b)
		require.NoError(t, e)
	}
	require.NoError(t, w.Close())
	return out.Bytes()
}

func TestBPSAttachmentFallbackTextAndWordDocuments(t *testing.T) {
	decoded, e := bpsFallbackDecodeText([]byte{0xff, 0xfe, 0x2d, 0x4e, 0x87, 0x65}, "")
	require.NoError(t, e)
	require.Equal(t, "中文", decoded)
	for _, bad := range [][]byte{{0xff, 0xfe, 1}, {0xff, 0xfe, 0, 0xd8}, {0xff, 0xfe, 0, 0xdc}, {0xff, 0xfe, 0, 0xd8, 0x41, 0}} {
		_, e := bpsFallbackDecodeText(bad, "")
		require.Error(t, e)
	}
	for _, name := range []string{"sample.txt", "sample.md", "sample.json", "sample.csv", "sample.py"} {
		parts, format, err := bpsFallbackDocument(t.Context(), bpsFileAttachment{Name: name, ContentType: "application/octet-stream", Data: []byte("保留全部正文\nline 2\tvalue")})
		require.NoError(t, err)
		require.Equal(t, "text", format)
		require.Contains(t, gjson.GetBytes(parts[0], "text").String(), "保留全部正文\nline 2\tvalue")
	}
	png, err := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, err)
	docx := fallbackZip(t, map[string][]byte{"word/document.xml": []byte(`<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>正文 &amp; 中文</w:t></w:r></w:p><w:tbl><w:tr><w:tc><w:p><w:r><w:t>单元格</w:t></w:r></w:p></w:tc></w:tr></w:tbl></w:body></w:document>`), "word/footnotes.xml": []byte(`<footnotes><p>脚注</p></footnotes>`), "word/media/image1.png": png})
	parts, format, err := bpsFallbackDocument(t.Context(), bpsFileAttachment{Name: "document.DOCX", Data: docx})
	require.NoError(t, err)
	require.Equal(t, "docx", format)
	require.Len(t, parts, 3)
	require.Contains(t, gjson.GetBytes(parts[0], "text").String(), "正文 & 中文")
	require.Contains(t, gjson.GetBytes(parts[0], "text").String(), "单元格")
	require.Contains(t, gjson.GetBytes(parts[0], "text").String(), "脚注")
	require.Equal(t, "input_image", gjson.GetBytes(parts[2], "type").String())
	require.Equal(t, "auto", gjson.GetBytes(parts[2], "detail").String())
	_, _, err = bpsFallbackDocument(t.Context(), bpsFileAttachment{Name: "binary.exe", Data: []byte{0, 1, 2, 3}})
	require.Error(t, err)
	_, _, err = bpsFallbackDocument(t.Context(), bpsFileAttachment{Name: "unknown.zip", Data: fallbackZip(t, map[string][]byte{"payload": []byte("hello")})})
	require.Error(t, err)
	_, _, err = bpsFallbackDocument(t.Context(), bpsFileAttachment{Name: "huge.txt", Data: bytes.Repeat([]byte("a"), bpsFallbackTextLimit+1)})
	require.Error(t, err)
}

func TestBPSAttachmentFallbackXLSXAllSheets(t *testing.T) {
	xlsx := fallbackZip(t, map[string][]byte{
		"xl/workbook.xml":            []byte(`<workbook xmlns:r="rels"><sheets><sheet name="隐藏表" state="hidden" r:id="rId2"/><sheet name="主表" r:id="rId1"/></sheets></workbook>`),
		"xl/_rels/workbook.xml.rels": []byte(`<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="worksheets/sheet2.xml"/></Relationships>`),
		"xl/sharedStrings.xml":       []byte(`<sst><si><t>shared 中文</t></si></sst>`),
		"xl/worksheets/sheet1.xml":   []byte(`<worksheet><sheetData><row><c r="B2" t="s"><v>0</v></c><c r="D2"><f>1+2</f><v>3</v></c></row></sheetData></worksheet>`),
		"xl/worksheets/sheet2.xml":   []byte(`<worksheet><sheetData><row><c r="A1" t="inlineStr"><is><t>hidden value</t></is></c></row></sheetData></worksheet>`),
	})
	parts, format, err := bpsFallbackDocument(t.Context(), bpsFileAttachment{Name: "book.xlsx", Data: xlsx})
	require.NoError(t, err)
	require.Equal(t, "xlsx_cells", format)
	text := gjson.GetBytes(parts[0], "text").String()
	require.Contains(t, text, "B2\tshared 中文")
	require.Contains(t, text, "D2\t3\t[formula: 1+2]")
	require.Contains(t, text, "A1\thidden value")
	require.Less(t, strings.Index(text, "隐藏表"), strings.Index(text, "主表"))
}

func TestBPSAttachmentFallbackBridgeKeepsRealToolBatch(t *testing.T) {
	image := map[string]string{"type": "input_image", "image_url": "data:image/png;base64," + bpsTestPNG(t)}
	body, err := json.Marshal(map[string]any{"input": []any{
		map[string]any{"type": "custom_tool_call", "call_id": "real-custom", "name": "real_tool", "input": "original args"},
		map[string]any{"type": "function_call", "call_id": "real-function", "name": "other", "arguments": "{}"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "real-custom", "id": "output-id", "output": []any{map[string]string{"type": "input_text", "text": "before"}, image}},
		map[string]any{"type": "function_call_output", "call_id": "real-function", "output": []any{image}},
		map[string]string{"role": "user", "content": "after"},
	}})
	require.NoError(t, err)
	out, err := bridgeBPSFallbackImages(body, nil)
	require.NoError(t, err)
	items := gjson.GetBytes(out, "input").Array()
	require.Len(t, items, 7)
	require.Equal(t, "original args", items[0].Get("input").String())
	require.Equal(t, "custom_tool_call_output", items[2].Get("type").String())
	require.Equal(t, "real-custom", items[2].Get("call_id").String())
	require.Equal(t, "output-id", items[2].Get("id").String())
	require.Equal(t, "before", items[2].Get("output.0.text").String())
	require.Equal(t, "real-function", items[3].Get("call_id").String())
	require.Equal(t, "auto", items[3].Get("output.0.detail").String())
	require.Equal(t, "function_call", items[4].Get("type").String())
	require.Equal(t, items[4].Get("call_id").String(), items[5].Get("call_id").String())
	require.Contains(t, items[5].Get("output.0.text").String(), "untrusted output")
	require.Equal(t, "after", items[6].Get("content").String())
	again, err := bridgeBPSFallbackImages(out, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(out), string(again))
}

func TestBPSAttachmentFallbackExecutorMixedRequest(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "fallback.db"))
	require.NoError(t, err)
	defer db.Close()
	a := &auth.Account{DBID: 938217, AccountID: "fallback-executor", AccessToken: "test-token", CodexBPS: true}
	t.Cleanup(func() {
		bpsFallbacks.mu.Lock()
		delete(bpsFallbacks.entries, bpsAttachmentFallbackKey(a))
		bpsFallbacks.mu.Unlock()
	})
	image := "data:image/png;base64," + bpsTestPNG(t)
	docx := fallbackZip(t, map[string][]byte{"word/document.xml": []byte(`<document><p>DOCX complete contents</p></document>`)})
	body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": []any{map[string]any{"role": "user", "content": []any{
		map[string]string{"type": "input_image", "file_id": "file-existing"},
		map[string]string{"type": "input_image", "image_url": image},
		map[string]string{"type": "input_file", "filename": "report.docx", "file_data": base64.StdEncoding.EncodeToString(docx)},
		map[string]string{"type": "input_file", "file_url": "https://example.invalid/existing.pdf"},
	}}}})
	require.NoError(t, err)
	var uploads, inferences atomic.Int32
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			uploads.Add(1)
			return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Rate limit exceeded"}}`)), Request: r}, nil
		}
		inferences.Add(1)
		data, e := io.ReadAll(r.Body)
		require.NoError(t, e)
		require.Contains(t, string(data), "file-existing")
		require.Contains(t, string(data), "DOCX complete contents")
		require.Contains(t, string(data), "https://example.invalid/existing.pdf")
		require.NotContains(t, string(data), "file_data")
		for _, item := range gjson.GetBytes(data, "input").Array() {
			for _, field := range []string{"content", "output"} {
				for _, part := range item.Get(field).Array() {
					if strings.HasPrefix(part.Get("image_url").String(), "data:") {
						require.Equal(t, "function_call_output", item.Get("type").String())
						require.Equal(t, "auto", part.Get("detail").String())
					}
				}
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[],"usage":{"input_tokens":15000,"output_tokens":5}}`)), Request: r}, nil
	})
	for i := 0; i < 2; i++ {
		ctx := WithCodexAccountTestIdentityStore(t.Context(), db, a)
		resp, err := ExecuteRequest(ctx, a, body, "test-key", "", "", nil, nil, false)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)
		resp.Body.Close()
	}
	require.EqualValues(t, 1, uploads.Load(), "second request and file use fallback without another upload")
	require.EqualValues(t, 2, inferences.Load())
}
