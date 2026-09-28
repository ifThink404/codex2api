package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSAccountUploadsShareLimitWithoutBlockingOtherAccounts(t *testing.T) {
	previousBPS := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previousBPS) })
	updateBPSConfig(t, func(s BPSConfig) BPSConfig {
		s.AttachmentAccountConcurrency = 1
		s.AttachmentInstanceConcurrency = 3
		return s
	})
	s := &bpsUploadScheduler{}
	a := context.WithValue(withBPSUploadRequest(t.Context()), bpsUploadAccountKey{}, int64(100))
	aOtherRequest, cancel := context.WithCancel(context.WithValue(withBPSUploadRequest(t.Context()), bpsUploadAccountKey{}, int64(100)))
	defer cancel()
	b := context.WithValue(withBPSUploadRequest(t.Context()), bpsUploadAccountKey{}, int64(200))
	releaseA, err := s.acquire(a, 1)
	require.NoError(t, err)
	defer releaseA()
	result := make(chan error, 1)
	go func() {
		release, err := s.acquire(aOtherRequest, 1)
		if err == nil {
			release()
		}
		result <- err
	}()
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.queues) == 1 }, time.Second, time.Millisecond)
	releaseB, err := s.acquire(b, 1)
	require.NoError(t, err, "account B can upload while another request on A queues")
	s.mu.Lock()
	require.Equal(t, 2, s.active, "queued A does not consume the third instance slot")
	require.Equal(t, map[int64]int{100: 1, 200: 1}, s.accounts)
	s.mu.Unlock()
	releaseB()
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	s.mu.Lock()
	require.Empty(t, s.queues)
	require.Equal(t, map[int64]int{100: 1}, s.accounts, "canceling a queued upload cannot release an active slot")
	s.mu.Unlock()
}

func TestBPSAccountUploadLimitIncreaseWakesQueue(t *testing.T) {
	previousBPS := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previousBPS) })
	updateBPSConfig(t, func(s BPSConfig) BPSConfig {
		s.AttachmentAccountConcurrency = 1
		s.AttachmentInstanceConcurrency = 3
		return s
	})
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), bpsUploadAccountKey{}, int64(300)))
	defer cancel()
	release, err := bpsUploads.acquire(withBPSUploadRequest(ctx), 1)
	require.NoError(t, err)
	defer release()
	result := make(chan error, 1)
	go func() {
		release, err := bpsUploads.acquire(withBPSUploadRequest(ctx), 1)
		if err == nil {
			release()
		}
		result <- err
	}()
	require.Eventually(t, func() bool { bpsUploads.mu.Lock(); defer bpsUploads.mu.Unlock(); return len(bpsUploads.queues) == 1 }, time.Second, time.Millisecond)
	updateBPSConfig(t, func(s BPSConfig) BPSConfig { s.AttachmentAccountConcurrency = 2; return s })
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("raising the account limit did not release the queued upload")
	}
}
