package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestBPSUploadRuntimeLimitReleasesQueuedAttachments(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.BPSAttachmentInstanceConcurrency = 1; return s })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release, err := bpsUploads.acquire(ctx, 1)
	require.NoError(t, err)
	defer release()
	done := make(chan error, 1)
	releaseSecond := make(chan struct{})
	finished := make(chan struct{})
	defer func() { cancel(); close(releaseSecond); <-finished }()
	go func() {
		defer close(finished)
		release, err := bpsUploads.acquire(ctx, 1)
		done <- err
		if err == nil {
			<-releaseSecond
			release()
		}
	}()
	require.Eventually(t, func() bool {
		bpsUploads.mu.Lock()
		defer bpsUploads.mu.Unlock()
		return len(bpsUploads.queues) == 1 && bpsUploads.active == 1
	}, time.Second, time.Millisecond)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.BPSAttachmentInstanceConcurrency = 2; return s })
	select {
	case err := <-done:
		require.NoError(t, err, "raising the limit wakes queued work before existing uploads finish")
	case <-time.After(time.Second):
		t.Fatal("queued attachment did not wake after saving a higher limit")
	}
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.BPSAttachmentInstanceConcurrency = 1; return s })
	bpsUploads.mu.Lock()
	active := bpsUploads.active
	bpsUploads.mu.Unlock()
	require.Equal(t, 2, active, "lowering a limit must not cancel uploads already admitted")
}

func TestBPSAttachmentWorkersUseSavedRequestLimit(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.BPSAttachmentRequestConcurrency = 3; return s })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var running, peak atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	done := make(chan error, 1)
	go func() {
		done <- runBPSAttachmentJobs(ctx, 15, func(ctx context.Context, _ int) error {
			n := running.Add(1)
			defer running.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			select {
			case <-gate:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	require.Eventually(t, func() bool { return running.Load() == 3 }, time.Second, time.Millisecond)
	release()
	require.NoError(t, <-done)
	require.EqualValues(t, 3, peak.Load())
}

func TestResinAccountConnectionLimitAndLiveReplacement(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(DefaultRuntimeSettings())
	a := &auth.Account{DBID: 8877001}
	b := &auth.Account{DBID: 8877002}
	t.Cleanup(func() {
		for _, account := range []*auth.Account{a, b} {
			if entry, ok := clientPool.LoadAndDelete(fmt.Sprintf("resin|%d", account.ID())); ok {
				releaseEvictedClient(entry.(*poolEntry).client)
			}
		}
	})
	client := getResinHTTPClient(a)
	require.Same(t, client, getResinHTTPClient(a))
	require.NotSame(t, client, getResinHTTPClient(b), "each account owns its connection pool")
	transport := client.Transport.(*http.Transport)
	require.Equal(t, 15, transport.MaxConnsPerHost)
	var active, peak atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 16)
	for range 16 {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			if err == nil {
				var response *http.Response
				response, err = client.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
			}
			done <- err
		}()
	}
	require.Eventually(t, func() bool { return active.Load() == 15 }, 2*time.Second, time.Millisecond)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.ResinAccountMaxConns = 7; return s })
	next := getResinHTTPClient(a)
	require.NotSame(t, client, next)
	require.Same(t, next, getResinHTTPClient(a))
	require.Equal(t, 7, next.Transport.(*http.Transport).MaxConnsPerHost)
	require.Equal(t, 15, transport.MaxConnsPerHost, "never mutate a transport being used concurrently")
	release()
	for range 16 {
		require.NoError(t, <-done, "existing requests finish when the connection limit changes")
	}
	require.EqualValues(t, 15, peak.Load())
}
