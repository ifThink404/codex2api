package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func uploadCooldownTestError(t *testing.T, status int, retryAfter, body string) error {
	t.Helper()
	return bpsAttachmentFailure(t.Context(), nil, "", "http", status, http.Header{"Retry-After": {retryAfter}}, []byte(body), nil)
}

func TestBPSUploadCooldownTTLAndFailureScope(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter, body string
		status                 int
		want                   time.Duration
	}{
		{name: "default", status: 429, want: time.Minute},
		{name: "seconds", status: 429, retryAfter: "90", want: 90 * time.Second},
		{name: "bounded", status: 429, retryAfter: "999999", want: 5 * time.Minute},
		{name: "date", status: 429, retryAfter: time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat), want: 2 * time.Minute},
		{name: "invalid", status: 429, retryAfter: "injected\r\nheader", want: time.Minute},
		{name: "server", status: 500},
		{name: "client", status: 422},
		{name: "policy", status: 429, body: `{"error":{"code":"cyber_policy","message":"blocked"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			a := &auth.Account{DBID: 99281, AccountID: "upload-cooldown-ttl"}
			before := time.Now()
			h.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, tc.status, tc.retryAfter, tc.body))
			until := bpsUploadCooldownState.get(bpsUploadCooldownKey(a), before)
			if tc.want == 0 {
				require.True(t, until.IsZero())
				return
			}
			require.InDelta(t, tc.want.Seconds(), until.Sub(before).Seconds(), 2)
			require.True(t, bpsUploadCooldownState.get(bpsUploadCooldownKey(a), until.Add(time.Nanosecond)).IsZero(), "cooldown expires automatically")
		})
	}
}

func TestBPSUploadCooldownRequestScopeAndCache(t *testing.T) {
	h := &Handler{}
	a := &auth.Account{DBID: 99282, AccountID: "upload-cooldown-scope"}
	other := &auth.Account{DBID: 99283, AccountID: "other-cooldown-account"}
	url := freshRetryImage(t)
	image := fmt.Sprintf(`{"type":"input_image","image_url":%q}`, url)
	body := []byte(`{"input":[{"role":"user","content":[` + image + `]}]}`)
	h.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, 429, "60", ""))
	blocked := func(body []byte, mode string, account *auth.Account) bool {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		bindBPSUploadRequestTest(h, c, body, false)
		return bpsUploadCooldownForRequest(c.Request.Context(), account, mode)
	}
	require.True(t, blocked(body, "bps", a))
	require.False(t, blocked(body, "native", a))
	require.False(t, blocked(body, "bps", other))
	for _, text := range []string{
		`{"input":"text only"}`,
		`{"input":[{"type":"function_call","arguments":"{\"type\":\"input_image\"}"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/image.png"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-existing"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-existing"}]}]}`,
	} {
		require.False(t, blocked([]byte(text), "bps", a), text)
	}
	for _, text := range []string{
		`{"input":[{"type":"custom_tool_call_output","call_id":"c1","output":[` + image + `]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_data":"YQ==","filename":"note.txt"}]}]}`,
		fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}]}`, url),
		fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}`, strings.SplitN(url, ",", 2)[1]),
	} {
		require.True(t, blocked([]byte(text), "bps", a), text)
	}
	_, _, err := prepareBPSUserImageAttachments(t.Context(), a, body, nil, func(context.Context, []byte, string) (string, error) { return "file-cooldown-cached", nil })
	require.NoError(t, err)
	require.False(t, blocked(body, "bps", a), "ready handles need no upload; inference can proceed")
	// A text or cached success does not clear the cooldown for a new attachment.
	newBody, _ := concurrentBPSImageBody(t, 998882, 1)
	require.True(t, blocked(newBody, "bps", a))
}

func TestBPSUploadCooldownSharedAndMonotonic(t *testing.T) {
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	t.Cleanup(func() { _ = backend.Close() })
	h1, h2 := &Handler{cache: backend}, &Handler{cache: backend}
	a := &auth.Account{DBID: 99284, AccountID: "upload-cooldown-shared"}
	key := bpsUploadCooldownKey(a)
	h1.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, 429, "120", ""))
	first := bpsUploadCooldownState.get(key, time.Now())
	h2.rememberBPSUploadFailure(t.Context(), a, uploadCooldownTestError(t, 429, "10", ""))
	require.True(t, bpsUploadCooldownState.get(key, time.Now()).Equal(first), "a later short hint cannot shorten the active cooldown")
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, freshRetryImage(t)))
	h3 := &Handler{cache: backend}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	bindBPSUploadRequestTest(h3, c, body, false)
	require.True(t, bpsUploadCooldownForRequest(c.Request.Context(), a, "bps"), "a fresh handler observes the shared cooldown")
	parts := bpsUploadParts(body, nil, false, false)
	attachmentKey := bpsUploadPartKey(a, parts[0])
	raw, err := json.Marshal(bpsAttachmentRecord{Version: 1, ID: "file-shared-cooldown", Expires: time.Now().Add(time.Minute)})
	require.NoError(t, err)
	require.NoError(t, backend.SetRuntime(t.Context(), bpsAttachmentNamespace, attachmentKey, raw, time.Minute))
	ctx := WithBPSAttachmentCache(c.Request.Context(), backend)
	require.False(t, bpsUploadCooldownForRequest(ctx, a, "bps"), "shared attachment hits need no new upload")
}

func TestBPSUploadCooldownCacheOutageAndConcurrency(t *testing.T) {
	backend := &failingBPSCache{sharedBPSMemory: sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}}
	t.Cleanup(func() { _ = backend.Close() })
	h := &Handler{cache: backend}
	a := &auth.Account{DBID: 99285, AccountID: "upload-cooldown-outage"}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 1; i <= 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bpsUploadCooldownState.remember(bpsUploadCooldownKey(a), now.Add(time.Duration(i)*time.Second), now)
		}()
	}
	wg.Wait()
	require.True(t, bpsUploadCooldownState.get(bpsUploadCooldownKey(a), now).Equal(now.Add(100*time.Second)))
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, freshRetryImage(t)))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	bindBPSUploadRequestTest(h, c, body, false)
	require.True(t, bpsUploadCooldownForRequest(c.Request.Context(), a, "bps"), "cache outage must not erase a local observation")
	for i := int64(1); i <= 10; i++ {
		require.False(t, bpsUploadCooldownForRequest(c.Request.Context(), &auth.Account{DBID: i}, "bps"))
	}
	require.EqualValues(t, 1, backend.reads.Load(), "only one failing shared lookup per request")
}
