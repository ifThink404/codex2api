package proxy

import (
	"bytes"
	"encoding/base64"
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

// bpsLadderImage returns a distinct valid PNG data URL tagged with marker.
func bpsLadderImage(t *testing.T, marker string) string {
	data, err := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, err)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(data, []byte(marker)...))
}

type bpsLadderUpstream struct {
	uploads   int
	responses []string
	// upload and respond decide each upstream answer.
	upload  func(body []byte) (int, string)
	respond func(n int, body []byte) (int, string)
}

func installBPSLadderUpstream(t *testing.T, account *auth.Account, u *bpsLadderUpstream) {
	installClaudeBoundaryTransport(t, account, func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		status, body := 200, `{"output":[]}`
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			u.uploads++
			status, body = 200, fmt.Sprintf(`{"openai_file_id":"file-ladder-%d"}`, u.uploads)
			if u.upload != nil {
				status, body = u.upload(raw)
			}
		} else {
			u.responses = append(u.responses, string(raw))
			if u.respond != nil {
				status, body = u.respond(len(u.responses), raw)
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
}

func bpsLadderBody(images ...string) []byte {
	var input []string
	for i, image := range images {
		if i > 0 {
			input = append(input, `{"role":"assistant","content":[{"type":"output_text","text":"seen"}]}`)
		}
		input = append(input, fmt.Sprintf(`{"role":"user","content":[{"type":"input_text","text":"look %d"},{"type":"input_image","image_url":%q}]}`, i, image))
	}
	return []byte(`{"model":"gpt-6-astra","input":[` + strings.Join(input, ",") + `]}`)
}

func runBPSLadderRequest(t *testing.T, account *auth.Account, body []byte) (*http.Response, error) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "ladder.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return executeBPSTestRequest(WithCodexAccountTestIdentityStore(t.Context(), db, account), account, body, "session", "", "", nil, nil, false)
}

func bpsLadderAccount(id int64) *auth.Account {
	return withBPSOverride(&auth.Account{DBID: id, AccountID: fmt.Sprintf("ladder-account-%d", id), AccessToken: "token"}, true)
}

func TestBPSImageLadderReuploadsCachedHandlesThenSucceeds(t *testing.T) {
	account := bpsLadderAccount(84001)
	image := bpsLadderImage(t, "reupload")
	u := &bpsLadderUpstream{}
	installBPSLadderUpstream(t, account, u)
	resp, err := runBPSLadderRequest(t, account, bpsLadderBody(image))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 1, u.uploads)

	// The cached handle is refused with a bare schema error: upload again.
	u.responses = nil
	u.respond = func(n int, _ []byte) (int, string) {
		if n == 1 {
			return http.StatusUnprocessableEntity, `{"detail":"invalid input"}`
		}
		return 200, `{"output":[]}`
	}
	resp, err = runBPSLadderRequest(t, account, bpsLadderBody(image))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, u.uploads)
	require.Len(t, u.responses, 2)
	require.Equal(t, "file-ladder-1", gjson.Get(u.responses[0], "input.1.content.1.file_id").String())
	require.Equal(t, "file-ladder-2", gjson.Get(u.responses[1], "input.1.content.1.file_id").String())
}

func TestBPSImageLadderDropsFreshImagesWithANote(t *testing.T) {
	account := bpsLadderAccount(84002)
	u := &bpsLadderUpstream{respond: func(n int, _ []byte) (int, string) {
		if n == 1 {
			return http.StatusBadRequest, `{"error":{"message":"Invalid image input"}}`
		}
		return 200, `{"output":[]}`
	}}
	installBPSLadderUpstream(t, account, u)
	resp, err := runBPSLadderRequest(t, account, bpsLadderBody(bpsLadderImage(t, "fresh")))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, u.uploads, "fresh handles are not uploaded again")
	require.Len(t, u.responses, 2)
	require.Equal(t, bpsImageOmittedRefused, gjson.Get(u.responses[1], "input.1.content.1.text").String())
	require.NotContains(t, u.responses[1], "file-ladder-")
	d := CodexBPSResponseDiagnostic(resp)
	require.NotNil(t, d.Images)
	require.Equal(t, 1, d.Images.OmittedAfterRefusal)
}

func TestBPSImageLadderStopsAfterDroppingImages(t *testing.T) {
	account := bpsLadderAccount(84003)
	u := &bpsLadderUpstream{respond: func(int, []byte) (int, string) {
		return http.StatusUnprocessableEntity, `{"detail":"invalid input"}`
	}}
	installBPSLadderUpstream(t, account, u)
	resp, err := runBPSLadderRequest(t, account, bpsLadderBody(bpsLadderImage(t, "stuck")))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.Len(t, u.responses, 2)
}

func TestBPSImageLadderIgnoresUnrelatedRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"error":{"message":"context length exceeded"}}`},
		{http.StatusInternalServerError, `{"error":{"message":"image service down"}}`},
	} {
		account := bpsLadderAccount(84004 + int64(tc.status))
		u := &bpsLadderUpstream{respond: func(int, []byte) (int, string) { return tc.status, tc.body }}
		installBPSLadderUpstream(t, account, u)
		resp, err := runBPSLadderRequest(t, account, bpsLadderBody(bpsLadderImage(t, "unrelated")))
		require.NoError(t, err)
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, tc.status, resp.StatusCode)
		require.Equal(t, bpsScrubbedErrorMessage, gjson.GetBytes(got, "error.message").String(), "the refusal body stays readable")
		require.Len(t, u.responses, 1)
	}
	// Bodies without images are never retried.
	account := bpsLadderAccount(84010)
	u := &bpsLadderUpstream{respond: func(int, []byte) (int, string) {
		return http.StatusUnprocessableEntity, `{"detail":"invalid input"}`
	}}
	installBPSLadderUpstream(t, account, u)
	resp, err := runBPSLadderRequest(t, account, []byte(`{"model":"gpt-6-astra","input":"hi"}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Len(t, u.responses, 1)
}

func TestBPSImageLadderHistoryUploadFailureBecomesANote(t *testing.T) {
	account := bpsLadderAccount(84011)
	u := &bpsLadderUpstream{upload: func(body []byte) (int, string) {
		if bytes.Contains(body, []byte("history-image")) {
			return http.StatusInternalServerError, `{"error":{"message":"upload failed"}}`
		}
		return 200, `{"openai_file_id":"file-ladder-current"}`
	}}
	installBPSLadderUpstream(t, account, u)
	resp, err := runBPSLadderRequest(t, account, bpsLadderBody(bpsLadderImage(t, "history-image"), bpsLadderImage(t, "current-image")))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, u.responses, 1)
	sent := gjson.Parse(u.responses[0])
	require.Equal(t, bpsImageOmittedUpload, sent.Get("input.1.content.1.text").String())
	require.Equal(t, "file-ladder-current", sent.Get("input.3.content.1.file_id").String())
}

func TestBPSImageLadderLatestTurnUploadFailureFailsTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		images []string
	}{
		{name: "latest turn", images: []string{bpsLadderImage(t, "history-ok"), bpsLadderImage(t, "current-image")}},
		// The same image repeated in the latest turn counts as a latest-turn image.
		{name: "repeated in latest turn", images: []string{bpsLadderImage(t, "current-image"), bpsLadderImage(t, "current-image")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := bpsLadderAccount(84020 + int64(len(tc.name)))
			u := &bpsLadderUpstream{upload: func(body []byte) (int, string) {
				if bytes.Contains(body, []byte("current-image")) {
					return http.StatusInternalServerError, `{"error":{"message":"upload failed"}}`
				}
				return 200, `{"openai_file_id":"file-ladder-history"}`
			}}
			installBPSLadderUpstream(t, account, u)
			_, err := runBPSLadderRequest(t, account, bpsLadderBody(tc.images...))
			require.Error(t, err)
			var upload *bpsAttachmentUploadError
			require.ErrorAs(t, err, &upload)
			require.Empty(t, u.responses)
		})
	}
}

func TestBPSImageRefusalClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusUnprocessableEntity, `{}`, true},
		{http.StatusBadRequest, `{"error":{"message":"Invalid image_url"}}`, true},
		{http.StatusBadRequest, `{"detail":"file is not supported"}`, true},
		{http.StatusBadRequest, `{"detail":{"error":{"message":"attachment rejected"}}}`, true},
		{http.StatusBadRequest, `{"error":{"message":"context length exceeded"}}`, false},
		{http.StatusNotFound, `{"error":{"message":"image not found"}}`, false},
		{http.StatusOK, `{}`, false},
	} {
		resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
		require.Equal(t, tc.want, bpsImageRefusal(resp), tc.body)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, tc.body, string(got))
	}
}
