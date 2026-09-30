package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/stretchr/testify/require"
)

type sharedBPSMemory struct{ *cache.MemoryTokenCache }

func (sharedBPSMemory) SharedAcrossInstances() bool { return true }

func newBPSLocalCache() *bpsAttachmentCache {
	return &bpsAttachmentCache{entries: make(map[string]*bpsAttachmentEntry)}
}

func TestBPSSharedAttachmentCacheAcrossInstancesAndAccounts(t *testing.T) {
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	t.Cleanup(func() { _ = backend.Close() })
	ctx := WithBPSAttachmentCache(t.Context(), backend)
	a := &auth.Account{DBID: 1, AccountID: "one"}
	b := &auth.Account{DBID: 2, AccountID: "two"}
	var uploads atomic.Int32
	upload := func() (string, error) { uploads.Add(1); return "file-shared", nil }
	key := bpsImageUploadKey(a, []byte("same image"))
	id, reused, err := newBPSLocalCache().resolve(ctx, key, upload)
	require.NoError(t, err)
	require.Equal(t, "file-shared", id)
	require.False(t, reused)
	timing := &bpsTimingDiagnostic{}
	second := newBPSLocalCache()
	id, reused, err = second.resolve(context.WithValue(ctx, bpsTimingContextKey{}, timing), key, upload)
	require.NoError(t, err)
	require.Equal(t, "file-shared", id)
	require.True(t, reused)
	require.EqualValues(t, 1, uploads.Load())
	require.Equal(t, 1, timing.values.SharedCacheHits)
	raw, found, err := backend.GetRuntime(ctx, bpsAttachmentNamespace, key)
	require.NoError(t, err)
	require.True(t, found)
	var record bpsAttachmentRecord
	require.NoError(t, json.Unmarshal(raw, &record))
	require.True(t, second.entries[key].expires.Equal(record.Expires), "loading another instance must not reset expiry")
	_, reused, err = newBPSLocalCache().resolve(ctx, bpsImageUploadKey(b, []byte("same image")), upload)
	require.NoError(t, err)
	require.False(t, reused)
	require.EqualValues(t, 2, uploads.Load())
}

func TestBPSSharedAttachmentSingleFlightAndCancellation(t *testing.T) {
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	t.Cleanup(func() { _ = backend.Close() })
	ctx := WithBPSAttachmentCache(t.Context(), backend)
	started, release := make(chan struct{}), make(chan struct{})
	var uploads atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, err := newBPSLocalCache().resolve(ctx, "shared", func() (string, error) { uploads.Add(1); close(started); <-release; return "file-shared", nil })
		if err != nil {
			t.Error(err)
		}
	}()
	<-started
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err := newBPSLocalCache().resolve(canceled, "shared", func() (string, error) { t.Error("canceled upload"); return "", nil })
	require.ErrorIs(t, err, context.Canceled)
	// A separate request has its own cache-failure circuit breaker.
	ctx = WithBPSAttachmentCache(t.Context(), backend)
	wg.Add(1)
	go func() {
		defer wg.Done()
		id, reused, err := newBPSLocalCache().resolve(ctx, "shared", func() (string, error) { uploads.Add(1); return "file-duplicate", nil })
		if err != nil || id != "file-shared" || !reused {
			t.Errorf("follower: id=%q reused=%v err=%v", id, reused, err)
		}
	}()
	close(release)
	wg.Wait()
	require.EqualValues(t, 1, uploads.Load())
}

type failingBPSCache struct {
	sharedBPSMemory
	reads atomic.Int32
}

func (b *failingBPSCache) GetRuntime(context.Context, string, string) (json.RawMessage, bool, error) {
	b.reads.Add(1)
	return nil, false, errors.New("unavailable")
}

func TestBPSSharedCacheOutageFallsBackOncePerRequest(t *testing.T) {
	backend := &failingBPSCache{sharedBPSMemory: sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}}
	t.Cleanup(func() { _ = backend.Close() })
	ctx := WithBPSAttachmentCache(t.Context(), backend)
	local := newBPSLocalCache()
	for _, key := range []string{"one", "two"} {
		id, reused, err := local.resolve(ctx, key, func() (string, error) { return "file-fallback", nil })
		require.NoError(t, err)
		require.False(t, reused)
		require.Equal(t, "file-fallback", id)
	}
	require.EqualValues(t, 1, backend.reads.Load())
}

func TestBPSSharedCacheExpiryAndConditionalInvalidation(t *testing.T) {
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	t.Cleanup(func() { _ = backend.Close() })
	ctx := WithBPSAttachmentCache(t.Context(), backend)
	put := func(id string, expires time.Time) {
		raw, _ := json.Marshal(bpsAttachmentRecord{Version: 1, ID: id, Expires: expires})
		require.NoError(t, backend.SetRuntime(ctx, bpsAttachmentNamespace, "key", raw, time.Hour))
	}
	put("file-expired", time.Now().Add(-time.Second))
	id, reused, err := newBPSLocalCache().resolve(ctx, "key", func() (string, error) { return "file-new", nil })
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, "file-new", id)
	forgetSharedBPSAttachment(ctx, "key", "file-expired")
	_, found, err := backend.GetRuntime(ctx, bpsAttachmentNamespace, "key")
	require.NoError(t, err)
	require.True(t, found, "late invalidation cannot delete the replacement")
	forgetSharedBPSAttachment(ctx, "key", "file-new")
	_, found, err = backend.GetRuntime(ctx, bpsAttachmentNamespace, "key")
	require.NoError(t, err)
	require.False(t, found)
}

func TestBPSAttachmentTTLDefaultAndConfigured(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"", 30 * time.Minute}, {"120", 2 * time.Hour}, {"0", 30 * time.Minute}, {"1441", 30 * time.Minute}, {"invalid", 30 * time.Minute}} {
		t.Setenv("CODEX_BPS_ATTACHMENT_CACHE_TTL_MINUTES", tc.value)
		require.Equal(t, tc.want, bpsAttachmentTTL())
	}
}

func TestBPSMissingAttachmentInvalidatesOnlyIdentifiedHandle(t *testing.T) {
	require.False(t, bpsErrorNamesAttachment("file-old-extra expired", "file-old"))
	require.True(t, bpsErrorNamesAttachment("file-old expired", "file-old"))
	backend := sharedBPSMemory{cache.NewMemory(1).(*cache.MemoryTokenCache)}
	t.Cleanup(func() { _ = backend.Close() })
	ctx := WithBPSAttachmentCache(t.Context(), backend)
	used := map[string]string{"test-expired-handle": "file-old-expired", "test-valid-handle": "file-still-valid"}
	for key, id := range used {
		_, _, err := bpsImages.resolve(ctx, key, func() (string, error) { return id, nil })
		require.NoError(t, err)
		t.Cleanup(func() { bpsImages.forget(key, id) })
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/responses", nil)
	require.NoError(t, err)
	resp := &http.Response{StatusCode: 400, Request: req, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"file_expired","message":"file-old-expired expired"}}`))}
	require.True(t, invalidateMissingBPSAttachments(resp, used))
	require.NoError(t, resp.Body.Close())
	for key, id := range used {
		_, found, err := backend.GetRuntime(ctx, bpsAttachmentNamespace, key)
		require.NoError(t, err)
		require.Equal(t, id == "file-still-valid", found)
	}
}
