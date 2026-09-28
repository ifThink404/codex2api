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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSAttachmentLeaderCancellationKeepsOtherWaiters(t *testing.T) {
	c := newBPSLocalCache()
	ctx, cancel := context.WithCancel(t.Context())
	started, release := make(chan struct{}), make(chan struct{})
	done, follower := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := c.resolveContext(ctx, "same", func(uploadCtx context.Context) (string, error) {
			close(started)
			select {
			case <-release:
				return "file-shared", nil
			case <-uploadCtx.Done():
				return "", uploadCtx.Err()
			}
		})
		done <- err
	}()
	<-started
	go func() {
		id, reused, err := c.resolveContext(t.Context(), "same", func(context.Context) (string, error) { return "file-wrong", nil })
		if err == nil && (id != "file-shared" || !reused) {
			err = errors.New("wrong shared upload result")
		}
		follower <- err
	}()
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.flights["same"].waiters == 2 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(release)
	require.NoError(t, <-follower)
	require.Empty(t, c.flights)
	require.Equal(t, "file-shared", c.entries["same"].id)
}

func TestBPSAttachmentLastWaiterCancellationJoinsUpload(t *testing.T) {
	c := newBPSLocalCache()
	ctx, cancel := context.WithCancel(t.Context())
	started, stopped, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := c.resolveContext(ctx, "cancel", func(uploadCtx context.Context) (string, error) {
			close(started)
			<-uploadCtx.Done()
			close(stopped)
			return "", uploadCtx.Err()
		})
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	select {
	case <-stopped:
	default:
		t.Fatal("upload outlived its last waiter")
	}
	require.Empty(t, c.flights)
	require.Empty(t, c.entries)
}

func TestBPSAttachmentLRUFailureAndExpiry(t *testing.T) {
	t.Setenv("CODEX_BPS_ATTACHMENT_CACHE_ENTRIES", "2")
	c := newBPSLocalCache()
	upload := func() (string, error) { return "file-ok", nil }
	for _, key := range []string{"a", "b", "a"} {
		_, _, err := c.resolve(t.Context(), key, upload)
		require.NoError(t, err)
	}
	_, _, err := c.resolve(t.Context(), "bad", func() (string, error) { return "", errors.New("failed") })
	require.Error(t, err)
	require.Len(t, c.entries, 2)
	require.Contains(t, c.entries, "a")
	require.Contains(t, c.entries, "b")
	_, _, err = c.resolve(t.Context(), "c", upload)
	require.NoError(t, err)
	require.Contains(t, c.entries, "a")
	require.NotContains(t, c.entries, "b")
	c.entries["a"].expires = time.Now().Add(-time.Second)
	_, reused, err := c.resolve(t.Context(), "a", upload)
	require.NoError(t, err)
	require.False(t, reused)
	require.Equal(t, len(c.entries), c.lru.Len())
	require.LessOrEqual(t, c.bytes, bpsLocalAttachmentBytes())
}

func TestBPSAttachmentPreparationSurvivesAccountSwitch(t *testing.T) {
	ctx, cleanup := withBPSAttachmentPreparation(t.Context())
	defer cleanup()
	timing := &bpsTimingDiagnostic{}
	ctx = context.WithValue(ctx, bpsTimingContextKey{}, timing)
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","detail":"original","image_url":"data:image/png;base64,%s"},{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, bpsTestPNG(t), bpsTestPNG(t)))
	var uploads atomic.Int32
	a := &auth.Account{DBID: 9988101, AccountID: "same-workspace"}
	b := &auth.Account{DBID: 9988102, AccountID: "same-workspace"}
	var handles []string
	for _, account := range []*auth.Account{a, b, a} {
		out, _, err := prepareBPSUserImageAttachments(ctx, account, body, nil, func(context.Context, []byte, string) (string, error) {
			return fmt.Sprintf("file-switch-%d", uploads.Add(1)), nil
		})
		require.NoError(t, err)
		id := gjson.GetBytes(out, "input.0.content.0.file_id").String()
		handles = append(handles, id)
		require.Equal(t, id, gjson.GetBytes(out, "input.0.content.1.file_id").String())
		require.Equal(t, "original", gjson.GetBytes(out, "input.0.content.0.detail").String())
	}
	require.EqualValues(t, 2, uploads.Load())
	require.Equal(t, handles[0], handles[2])
	require.NotEqual(t, handles[0], handles[1])
	require.Equal(t, 1, timing.values.AttachmentDecodes)
	require.Equal(t, 1, timing.values.AttachmentHashes)
	require.Equal(t, 2, timing.values.PreparationReuses)
	require.Equal(t, 3, timing.values.AttachmentDeduplicated)
	require.Contains(t, string(body), "image_url")
}

func TestBPSPreparedFileNamesAndTypesKeepSeparateIdentity(t *testing.T) {
	ctx, cleanup := withBPSAttachmentPreparation(t.Context())
	defer cleanup()
	a, err := prepareBPSAttachment(ctx, "file", "aGVsbG8=", "one.txt")
	require.NoError(t, err)
	b, err := prepareBPSAttachment(ctx, "file", "aGVsbG8=", "two.txt")
	require.NoError(t, err)
	c, err := prepareBPSAttachment(ctx, "file", "data:application/pdf;base64,aGVsbG8=", "one.txt")
	require.NoError(t, err)
	require.NotEqual(t, a.identity, b.identity)
	require.NotEqual(t, a.identity, c.identity)
	account := &auth.Account{DBID: 99, AccountID: "workspace"}
	require.Equal(t, bpsFileUploadKey(account, a.file), bpsPreparedUploadKey(account, "file", a))
}

func TestBPSUploadSchedulerFairnessAndCancellation(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.BPSAttachmentInstanceConcurrency = 1; return s })
	s := &bpsUploadScheduler{}
	a, b := withBPSUploadRequest(t.Context()), withBPSUploadRequest(t.Context())
	release, err := s.acquire(a, 1)
	require.NoError(t, err)
	type result struct {
		id      string
		release func()
	}
	results := make(chan result, 3)
	enqueue := func(ctx context.Context, id string, count int) {
		go func() {
			release, err := s.acquire(ctx, 1)
			if err == nil {
				results <- result{id, release}
			}
		}()
		require.Eventually(t, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			n := 0
			for _, q := range s.queues {
				n += len(q.tickets)
			}
			return n == count
		}, time.Second, time.Millisecond)
	}
	enqueue(a, "a1", 1)
	enqueue(a, "a2", 2)
	enqueue(b, "b1", 3)
	release()
	for _, expected := range []string{"a1", "b1", "a2"} {
		r := <-results
		require.Equal(t, expected, r.id)
		r.release()
	}
	release, err = s.acquire(a, 1)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(b)
	done := make(chan error, 1)
	go func() {
		r, err := s.acquire(canceled, 1)
		if err == nil {
			r()
		}
		done <- err
	}()
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.queues) == 1 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	release()
	require.Zero(t, s.active)
	require.Zero(t, s.bytes)
	require.Empty(t, s.queues)
}

func TestBPSUploadSchedulerByteBudgetAndOversizeProgress(t *testing.T) {
	t.Setenv("CODEX_BPS_ATTACHMENT_BUFFER_MIB", "64")
	s := &bpsUploadScheduler{}
	release, err := s.acquire(t.Context(), 64<<20)
	require.NoError(t, err)
	done := make(chan func(), 1)
	go func() {
		r, err := s.acquire(t.Context(), 128<<20)
		if err == nil {
			done <- r
		}
	}()
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.queues) == 1 }, time.Second, time.Millisecond)
	release()
	r := <-done
	require.Equal(t, 1, s.active)
	r()
	require.Zero(t, s.bytes)
}

func TestBPSAttachmentBufferAdmissionAndCancel(t *testing.T) {
	t.Setenv("CODEX_BPS_ATTACHMENT_BUFFER_MIB", "64")
	p := &bpsAttachmentBufferPool{}
	release, err := p.acquire(t.Context(), 32<<20)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		r, err := p.acquire(ctx, 1)
		if err == nil {
			r()
		}
		done <- err
	}()
	require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return len(p.queue) == 1 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	release()
	require.Zero(t, p.bytes)
	require.Empty(t, p.queue)
}

func TestBPSPreparationRetentionIsBoundedAndReleased(t *testing.T) {
	ctx, cleanup := withBPSAttachmentPreparation(t.Context())
	defer cleanup()
	m := ctx.Value(bpsAttachmentPreparationKey{}).(*bpsAttachmentPreparation)
	large := base64.StdEncoding.EncodeToString(make([]byte, 4<<20))
	_, err := prepareBPSAttachment(ctx, "file", large, "large.bin")
	require.NoError(t, err)
	require.Empty(t, m.items, "large files remain supported without retaining them across retries")
	_, err = prepareBPSAttachment(ctx, "file", "aGVsbG8=", "small.txt")
	require.NoError(t, err)
	require.Len(t, m.items, 1)
	cleanup()
	require.Empty(t, m.items)
	require.True(t, m.closed)
}

func TestBPSAttachmentLimitValidation(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, value := range []int{0, -1, 99999999} {
		UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
			s.BPSAttachmentRequestConcurrency = value
			s.BPSAttachmentInstanceConcurrency = value
			return s
		})
		require.Equal(t, 15, bpsRequestUploadLimit())
		require.Equal(t, 64, bpsInstanceUploadLimit())
	}
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.BPSAttachmentRequestConcurrency = 8; return s })
	require.Equal(t, 8, bpsRequestUploadLimit())
}

func BenchmarkBPSAttachmentCacheHit(b *testing.B) {
	for _, capacity := range []int{256, 4096, 16384} {
		b.Run(fmt.Sprint(capacity), func(b *testing.B) {
			b.Setenv("CODEX_BPS_ATTACHMENT_CACHE_ENTRIES", fmt.Sprint(capacity))
			c := newBPSLocalCache()
			for i := 0; i < capacity; i++ {
				_, _, err := c.resolve(b.Context(), fmt.Sprint(i), func() (string, error) { return "file-benchmark", nil })
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, _, err := c.resolveContext(b.Context(), "0", func(context.Context) (string, error) { return "file-unexpected", nil })
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestBPSUploadStreamsMultipartWithReplayableLength(t *testing.T) {
	data := bytes.Repeat([]byte("immutable-file-data\x00"), 65536)
	client := &http.Client{Transport: claudeBoundaryRoundTripper(func(r *http.Request) (*http.Response, error) {
		replay, err := r.GetBody()
		require.NoError(t, err)
		raw, err := io.ReadAll(replay)
		require.NoError(t, err)
		require.NoError(t, replay.Close())
		require.EqualValues(t, len(raw), r.ContentLength)
		reader, err := r.MultipartReader()
		require.NoError(t, err)
		part, err := reader.NextPart()
		require.NoError(t, err)
		require.Equal(t, "example.bin", part.FileName())
		got, err := io.ReadAll(part)
		require.NoError(t, err)
		require.Equal(t, data, got)
		_, err = reader.NextPart()
		require.ErrorIs(t, err, io.EOF)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-streamed"}`))}, nil
	})}
	id, err := uploadBPSAttachment(t.Context(), client, http.Header{}, bpsFileAttachment{Data: data, Name: "example.bin", ContentType: "application/octet-stream"})
	require.NoError(t, err)
	require.Equal(t, "file-streamed", id)
}

func TestBPSFileUploadsBoundedAndOrdered(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var parts []any
	for i := 0; i < 31; i++ {
		parts = append(parts, map[string]any{"type": "input_file", "filename": fmt.Sprintf("part-%d.txt", i), "file_data": base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(i)))})
	}
	body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": parts}}})
	require.NoError(t, err)
	started, release := make(chan struct{}, 31), make(chan struct{})
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	var active, peak atomic.Int32
	go func() {
		out, _, err := prepareBPSFileAttachments(ctx, &auth.Account{DBID: 9988201, AccountID: NewUpstreamSessionUUID()}, body, nil, func(uploadCtx context.Context, file bpsFileAttachment) (string, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-release:
				return "file-part-" + string(file.Data), nil
			case <-uploadCtx.Done():
				return "", uploadCtx.Err()
			}
		})
		done <- result{out, err}
	}()
	for i := 0; i < 15; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	r := <-done
	require.NoError(t, r.err)
	require.EqualValues(t, 15, peak.Load())
	require.Zero(t, active.Load())
	for i := 0; i < 31; i++ {
		part := gjson.GetBytes(r.body, fmt.Sprintf("input.0.content.%d", i))
		require.Equal(t, fmt.Sprintf("file-part-%d", i), part.Get("file_id").String())
		require.False(t, part.Get("file_data").Exists())
	}
	require.Contains(t, string(body), "file_data")
}
