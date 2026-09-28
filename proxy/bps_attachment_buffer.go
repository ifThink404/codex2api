package proxy

import (
	"context"
	"sync"
	"time"
)

type bpsAttachmentBufferPool struct {
	mu    sync.Mutex
	bytes int64
	queue []*bpsUploadTicket
}

var bpsAttachmentBuffers bpsAttachmentBufferPool

// Reserve before decoding, including conservative processing headroom. This bounds
// buffers held by tasks waiting for cache/lease/network slots, not just uploads
// that already reached HTTP. Request bodies and the bounded preparation memo
// are separate allocations. One oversized attachment can progress alone.
func (p *bpsAttachmentBufferPool) dispatch() {
	for len(p.queue) > 0 {
		t := p.queue[0]
		if p.bytes > 0 && p.bytes+t.bytes > bpsUploadBufferLimit() {
			return
		}
		p.queue[0] = nil
		p.queue = p.queue[1:]
		p.bytes += t.bytes
		t.granted = true
		close(t.ready)
	}
}

func (p *bpsAttachmentBufferPool) acquire(ctx context.Context, encodedBytes int) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	weight := int64(encodedBytes)*2 + 1024
	t := &bpsUploadTicket{ready: make(chan struct{}), bytes: weight}
	started := time.Now()
	p.mu.Lock()
	p.queue = append(p.queue, t)
	p.dispatch()
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		p.mu.Lock()
		if t.granted {
			p.bytes -= weight
		} else {
			for i, pending := range p.queue {
				if pending == t {
					copy(p.queue[i:], p.queue[i+1:])
					p.queue[len(p.queue)-1] = nil
					p.queue = p.queue[:len(p.queue)-1]
					break
				}
			}
		}
		p.dispatch()
		p.mu.Unlock()
		return nil, ctx.Err()
	case <-t.ready:
		bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.BufferWaitMS += time.Since(started).Milliseconds() })
		return func() { p.mu.Lock(); p.bytes -= weight; p.dispatch(); p.mu.Unlock() }, nil
	}
}
