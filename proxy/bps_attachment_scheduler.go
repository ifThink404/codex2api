package proxy

import (
	"context"
	"sync"
	"time"
)

type bpsUploadQueueRequestKey struct{}
type bpsUploadQueueRequest struct{ marker byte }
type bpsUploadTicket struct {
	ready   chan struct{}
	bytes   int64
	granted bool
}
type bpsUploadQueue struct {
	request *bpsUploadQueueRequest
	tickets []*bpsUploadTicket
}
type bpsUploadScheduler struct {
	mu     sync.Mutex
	queues []*bpsUploadQueue
	active int
	bytes  int64
}

var bpsUploads bpsUploadScheduler

func withBPSUploadRequest(ctx context.Context) context.Context {
	if _, ok := ctx.Value(bpsUploadQueueRequestKey{}).(*bpsUploadQueueRequest); ok {
		return ctx
	}
	return context.WithValue(ctx, bpsUploadQueueRequestKey{}, &bpsUploadQueueRequest{})
}

// Round-robin across waiting business requests. A large upload at the head is
// allowed to drain active work rather than starving behind endless small files.
func (s *bpsUploadScheduler) dispatch() {
	for len(s.queues) > 0 && s.active < bpsInstanceUploadLimit() {
		q := s.queues[0]
		t := q.tickets[0]
		if s.bytes+t.bytes > bpsUploadBufferLimit() && s.active > 0 {
			return
		}
		s.queues[0] = nil
		s.queues = s.queues[1:]
		q.tickets[0] = nil
		q.tickets = q.tickets[1:]
		if len(q.tickets) > 0 {
			s.queues = append(s.queues, q)
		}
		t.granted = true
		s.active++
		s.bytes += t.bytes
		close(t.ready)
	}
}

func (s *bpsUploadScheduler) acquire(ctx context.Context, bytes int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request, _ := ctx.Value(bpsUploadQueueRequestKey{}).(*bpsUploadQueueRequest)
	if request == nil {
		request = &bpsUploadQueueRequest{}
	}
	t := &bpsUploadTicket{ready: make(chan struct{}), bytes: bytes}
	started := time.Now()
	s.mu.Lock()
	var queue *bpsUploadQueue
	for _, q := range s.queues {
		if q.request == request {
			queue = q
			break
		}
	}
	if queue == nil {
		queue = &bpsUploadQueue{request: request}
		s.queues = append(s.queues, queue)
	}
	queue.tickets = append(queue.tickets, t)
	s.dispatch()
	s.mu.Unlock()
	release := func() { s.mu.Lock(); s.active--; s.bytes -= bytes; s.dispatch(); s.mu.Unlock() }
	select {
	case <-ctx.Done():
		s.mu.Lock()
		if t.granted {
			s.active--
			s.bytes -= bytes
		} else {
			for i, q := range s.queues {
				if q != queue {
					continue
				}
				for j, ticket := range q.tickets {
					if ticket == t {
						copy(q.tickets[j:], q.tickets[j+1:])
						q.tickets[len(q.tickets)-1] = nil
						q.tickets = q.tickets[:len(q.tickets)-1]
						break
					}
				}
				if len(q.tickets) == 0 {
					copy(s.queues[i:], s.queues[i+1:])
					s.queues[len(s.queues)-1] = nil
					s.queues = s.queues[:len(s.queues)-1]
				}
				break
			}
		}
		s.dispatch()
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-t.ready:
		bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) {
			v.UploadQueueMS += time.Since(started).Milliseconds()
			v.InstanceUploadLimit = bpsInstanceUploadLimit()
		})
		return release, nil
	}
}
