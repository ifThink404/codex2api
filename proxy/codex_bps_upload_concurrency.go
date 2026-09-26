package proxy

import (
	"context"
	"sync"
	"sync/atomic"
)

const bpsAttachmentUploadConcurrency = 15

// Workers produce independent results; JSON rewriting stays on the caller
// goroutine in source order. A failure cancels and joins all in-flight workers.
func runBPSAttachmentJobs(ctx context.Context, count int, run func(context.Context, int) error) error {
	if count == 0 {
		return nil
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	var once sync.Once
	var firstErr error
	var wg sync.WaitGroup
	for worker := 0; worker < min(count, bpsAttachmentUploadConcurrency); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for workCtx.Err() == nil {
				index := int(next.Add(1)) - 1
				if index >= count {
					return
				}
				if err := run(workCtx, index); err != nil {
					once.Do(func() { firstErr = err; cancel() })
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
