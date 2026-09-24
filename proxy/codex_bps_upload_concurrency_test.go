package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Unique, valid PNGs make every position a cache miss without external traffic.
func concurrentBPSImageBody(t *testing.T, seed, count int) ([]byte, map[string]int) {
	t.Helper()
	original, err := base64.StdEncoding.DecodeString(bpsTestPNG(t))
	require.NoError(t, err)
	content, positions := []any{}, map[string]int{}
	for i := 0; i < count; i++ {
		data := append(append([]byte(nil), original...), []byte(fmt.Sprintf("-%d-%d", seed, i))...)
		positions[string(data)] = i
		content = append(content, map[string]any{"type": "input_image", "detail": "original", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)})
	}
	body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "id": "original", "content": content}}})
	require.NoError(t, err)
	return body, positions
}

func TestBPSImageUploadsHundredPerRequestWithoutAccountCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a := &auth.Account{DBID: 89011, AccountID: "concurrent-uploads"}
	started, release := make(chan int, 402), make(chan struct{})
	finished := make(chan error, 2)
	var active [2]atomic.Int32
	var peak [2]atomic.Int32
	for request := 0; request < 2; request++ {
		body, _ := concurrentBPSImageBody(t, request, 201)
		go func() {
			_, _, err := prepareBPSUserImageAttachments(ctx, a, body, nil, func(workCtx context.Context, _ []byte, _ string) (string, error) {
				n := active[request].Add(1)
				defer active[request].Add(-1)
				for old := peak[request].Load(); n > old; old = peak[request].Load() {
					if peak[request].CompareAndSwap(old, n) {
						break
					}
				}
				started <- request
				select {
				case <-release:
					return "file-uploaded", nil
				case <-workCtx.Done():
					return "", workCtx.Err()
				}
			})
			finished <- err
		}()
	}
	// Both requests, on the SAME account, must reach 100 blocked uploads.
	counts := [2]int{}
	for i := 0; i < 200; i++ {
		select {
		case request := <-started:
			counts[request]++
		case <-ctx.Done():
			t.Fatal("uploads did not overlap: ", counts)
		}
	}
	require.Equal(t, [2]int{100, 100}, counts)
	close(release)
	for i := 0; i < 2; i++ {
		require.NoError(t, <-finished)
	}
	for request := 0; request < 2; request++ {
		require.EqualValues(t, 100, peak[request].Load())
		require.Zero(t, active[request].Load())
	}
}

func TestBPSImageUploadsOutOfOrderKeepOriginalPositions(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	body, positions := concurrentBPSImageBody(t, 1234, 3)
	original := append([]byte(nil), body...)
	started, completed := make(chan int, 3), make(chan int, 3)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	type result struct {
		body []byte
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		out, _, err := prepareBPSUserImageAttachments(ctx, &auth.Account{DBID: 89012}, body, nil, func(workCtx context.Context, data []byte, _ string) (string, error) {
			i := positions[string(data)]
			started <- i
			select {
			case <-releases[i]:
				completed <- i
				return fmt.Sprintf("file-position-%d", i), nil
			case <-workCtx.Done():
				return "", workCtx.Err()
			}
		})
		finished <- result{out, err}
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for i := 2; i >= 0; i-- {
		close(releases[i])
		require.Equal(t, i, <-completed)
	}
	r := <-finished
	require.NoError(t, r.err)
	require.Equal(t, original, body)
	require.Equal(t, "original", gjson.GetBytes(r.body, "input.0.id").String())
	for i := 0; i < 3; i++ {
		part := gjson.GetBytes(r.body, fmt.Sprintf("input.0.content.%d", i))
		require.Equal(t, fmt.Sprintf("file-position-%d", i), part.Get("file_id").String())
		require.Equal(t, "original", part.Get("detail").String())
		require.False(t, part.Get("image_url").Exists())
	}
}

func TestBPSImageUploadFailureCancelsAndJoinsWorkers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started, fail := make(chan struct{}, 100), make(chan struct{})
	finished := make(chan error, 1)
	var active, calls atomic.Int32
	expected := errors.New("attachment upload failed")
	go func() {
		finished <- runBPSAttachmentJobs(ctx, 300, func(workCtx context.Context, i int) error {
			active.Add(1)
			calls.Add(1)
			defer active.Add(-1)
			started <- struct{}{}
			if i == 0 {
				select {
				case <-fail:
					return expected
				case <-workCtx.Done():
					return workCtx.Err()
				}
			}
			<-workCtx.Done()
			return workCtx.Err()
		})
	}()
	for i := 0; i < 100; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(fail)
	require.ErrorIs(t, <-finished, expected)
	require.Zero(t, active.Load(), "must join before the handler returns")
	require.EqualValues(t, 100, calls.Load(), "queued work must not start after failure")
}
