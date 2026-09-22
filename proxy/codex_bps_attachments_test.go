package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSUserImageAttachmentsExecutor(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "images.db"))
	require.NoError(t, err)
	defer db.Close()
	imageData := bpsTestPNG(t)
	decoded, err := base64.StdEncoding.DecodeString(imageData)
	require.NoError(t, err)
	for n := int64(81001); n <= 81002; n++ {
		a := &auth.Account{DBID: n, AccountID: fmt.Sprint(n), AccessToken: "account-only-token", CodexBPS: true}
		uploads, responses := 0, 0
		fileID := fmt.Sprintf("file-private-%d", n)
		installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "Bearer account-only-token", r.Header.Get("Authorization"))
			require.Equal(t, a.AccountID, r.Header.Get("Chatgpt-Account-Id"))
			require.Empty(t, r.Header.Get(codexTurnStateHeader))
			if strings.HasSuffix(r.URL.Path, "/attachments") {
				uploads++
				reader, e := r.MultipartReader()
				require.NoError(t, e)
				part, e := reader.NextPart()
				require.NoError(t, e)
				require.Equal(t, "file", part.FormName())
				require.Equal(t, "image.png", part.FileName())
				require.Equal(t, "image/png", part.Header.Get("Content-Type"))
				data, e := io.ReadAll(part)
				require.NoError(t, e)
				require.Equal(t, decoded, data)
				_, e = reader.NextPart()
				require.ErrorIs(t, e, io.EOF)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"` + fileID + `"}`)), Request: r}, nil
			}
			responses++
			body, e := io.ReadAll(r.Body)
			require.NoError(t, e)
			user := gjson.GetBytes(body, "input.1")
			require.Equal(t, "user", user.Get("role").String())
			require.Equal(t, "user-original", user.Get("id").String())
			require.Equal(t, "9007199254740993", user.Get("large_integer").Raw)
			require.Equal(t, "before", user.Get("content.0.text").String())
			require.Equal(t, "between", user.Get("content.2.text").String())
			for _, idx := range []string{"1", "3"} {
				require.Equal(t, fileID, user.Get("content."+idx+".file_id").String())
				require.False(t, user.Get("content."+idx+".image_url").Exists())
			}
			require.Equal(t, "original", user.Get("content.1.detail").String())
			require.Equal(t, "low", user.Get("content.3.detail").String())
			require.Equal(t, "call_original", gjson.GetBytes(body, "input.2.call_id").String())
			require.Equal(t, "input_text", gjson.GetBytes(body, "input.2.output.0.type").String())
			require.Equal(t, fileID, gjson.GetBytes(body, "input.3.content.1.file_id").String())
			require.Equal(t, "unchanged-business-data", gjson.GetBytes(body, "input.4.content").String())
			require.NotContains(t, string(body), imageData)
			require.Len(t, gjson.GetBytes(body, "input").Array(), 5)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_test","output":[]}`)), Request: r}, nil
		})
		body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","id":"user-original","large_integer":9007199254740993,"content":[{"type":"input_text","text":"before"},{"type":"input_image","image_url":"data:application/octet-stream;base64,` + imageData + `","detail":"original"},{"type":"input_text","text":"between"},{"type":"input_image","image_url":"data:image/png;base64,` + imageData + `","detail":"low"}]},{"type":"function_call_output","call_id":"call_original","output":[{"type":"input_image","image_url":"data:image/png;base64,` + imageData + `"}]},{"role":"assistant","content":"unchanged-business-data"}]}`)
		original := bytes.Clone(body)
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
			require.NotNil(t, d)
			if round == 0 {
				require.Equal(t, 1, d.Images.Uploaded)
				require.Equal(t, 2, d.Images.UploadReused)
			} else {
				require.Zero(t, d.Images.Uploaded)
				require.Equal(t, 3, d.Images.UploadReused)
			}
			logged, e := json.Marshal(d)
			require.NoError(t, e)
			require.NotContains(t, string(logged), fileID)
			require.NotContains(t, string(logged), imageData)
			require.Equal(t, original, body)
		}
		require.Equal(t, 1, uploads)
		require.Equal(t, 3, responses)
	}
}

func TestBPSAttachmentCacheConcurrentBoundedAndCancelable(t *testing.T) {
	c := &bpsAttachmentCache{entries: make(map[string]*bpsAttachmentEntry)}
	var uploads atomic.Int32
	start, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		id, _, err := c.resolve(t.Context(), "shared", func() (string, error) { uploads.Add(1); close(start); <-release; return "file-shared", nil })
		if err != nil || id != "file-shared" {
			t.Errorf("leader: %v %s", err, id)
		}
	}()
	<-start
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := c.resolve(ctx, "shared", func() (string, error) { t.Error("unexpected upload"); return "", nil })
	require.ErrorIs(t, err, context.Canceled)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, reused, err := c.resolve(t.Context(), "shared", func() (string, error) { uploads.Add(1); return "file-wrong", nil })
			if err != nil || !reused || id != "file-shared" {
				t.Errorf("waiter: %v %t %s", err, reused, id)
			}
		}()
	}
	close(release)
	wg.Wait()
	require.EqualValues(t, 1, uploads.Load())
	c.entries["shared"].expires = time.Now().Add(-time.Second)
	id, reused, err := c.resolve(t.Context(), "shared", func() (string, error) { return "file-new", nil })
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, "file-new", id)
	_, _, err = c.resolve(t.Context(), "failed", func() (string, error) { return "", errors.New("temporary") })
	require.Error(t, err)
	require.NotContains(t, c.entries, "failed")
	for i := 0; i < bpsAttachmentCacheLimit+5; i++ {
		_, _, err = c.resolve(t.Context(), fmt.Sprint(i), func() (string, error) { return "file-test", nil })
		require.NoError(t, err)
	}
	require.Len(t, c.entries, bpsAttachmentCacheLimit)
}

func TestBPSAttachmentInvalidInputAndUploadErrors(t *testing.T) {
	a := &auth.Account{DBID: 82001, AccountID: "validation-account"}
	for _, url := range []string{"data:image/png,not-base64", "data:image/png;base64,!!!", "data:text/plain;base64,aGVsbG8="} {
		body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": url}}}}})
		_, _, err := prepareBPSUserImageAttachments(t.Context(), a, body, nil, func(context.Context, []byte, string) (string, error) {
			t.Fatal("invalid input uploaded")
			return "", nil
		})
		var e *Error
		require.ErrorAs(t, err, &e)
		require.Equal(t, 400, e.HTTPStatus)
	}
	data := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"caller-file"},{"type":"input_image","image_url":"https://private.invalid/image"}]},{"type":"function_call_output","output":"{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,invalid\"}"}]}`)
	out, _, err := prepareBPSUserImageAttachments(t.Context(), a, data, nil, func(context.Context, []byte, string) (string, error) {
		t.Fatal("unrelated reference uploaded")
		return "", nil
	})
	require.NoError(t, err)
	require.Equal(t, data, out)
	for _, status := range []int{302, 400, 401, 422, 429, 500} {
		client := &http.Client{Transport: claudeBoundaryRoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://untrusted.invalid/"}}, Body: io.NopCloser(strings.NewReader(`{"message":"private-file-token"}`)), Request: r}, nil
		})}
		_, err := uploadBPSImage(t.Context(), client, http.Header{"Authorization": {"Bearer secret"}}, []byte("test"), "image/png")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private-file-token")
		require.NotContains(t, err.Error(), "secret")
		require.NotContains(t, err.Error(), "untrusted.invalid")
	}
}

func TestBPSAttachmentExpiredReferenceRetriesOnce(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "expired.db"))
	require.NoError(t, err)
	defer db.Close()
	a := &auth.Account{DBID: 83001, AccountID: "expired-image-account", AccessToken: "token", CodexBPS: true}
	uploads, responses := 0, 0
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{"output":[]}`
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			uploads++
			body = fmt.Sprintf(`{"openai_file_id":"file-version-%d"}`, uploads)
		} else {
			responses++
			raw, e := io.ReadAll(r.Body)
			require.NoError(t, e)
			require.Equal(t, fmt.Sprintf("file-version-%d", uploads), gjson.GetBytes(raw, "input.1.content.0.file_id").String())
			// Even when the second handle is also missing, do not loop forever.
			status = 400
			body = fmt.Sprintf(`{"error":{"code":"file_not_found","message":"File file-version-%d not found"}}`, uploads)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[]}]}`)
	body, _ = sjson.SetBytes(body, "input.0.content", []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + bpsTestPNG(t)}})
	ctx := WithCodexAccountTestIdentityStore(t.Context(), db, a)
	resp, err := ExecuteRequest(ctx, a, body, "session", "", "", nil, nil, false)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 400, resp.StatusCode)
	require.Equal(t, 2, uploads)
	require.Equal(t, 2, responses)
	for _, status := range []int{400, 422, 500} {
		raw := `{"error":{"message":"unrelated error"}}`
		resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(raw))}
		require.False(t, invalidateMissingBPSAttachments(resp, map[string]string{"unused": "file-id"}))
		got, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		require.Equal(t, raw, string(got))
		resp.Body.Close()
	}
}
