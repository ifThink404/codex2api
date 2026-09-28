package proxy

import (
	"context"
	"sync"
	"time"
)

type bpsUploadQueueRequestKey struct{}
type bpsUploadAccountKey struct{}
type bpsUploadQueueRequest struct{ marker byte }
type bpsUploadTicket struct {
	ready     chan struct{}
	bytes     int64
	granted   bool
	accountID int64
}
type bpsUploadQueue struct {
	request *bpsUploadQueueRequest
	tickets []*bpsUploadTicket
}
type bpsUploadScheduler struct {
	mu       sync.Mutex
	queues   []*bpsUploadQueue
	active   int
	bytes    int64
	accounts map[int64]int
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
	blocked := 0
	for len(s.queues) > 0 && s.active < bpsInstanceUploadLimit() && blocked < len(s.queues) {
		q := s.queues[0]
		t := q.tickets[0]
		if s.accounts[t.accountID] >= bpsAccountUploadLimit() {
			// A saturated account must not block other accounts in the queue.
			copy(s.queues, s.queues[1:])
			s.queues[len(s.queues)-1] = q
			blocked++
			continue
		}
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
		blocked = 0
		if s.accounts == nil {
			s.accounts = make(map[int64]int)
		}
		s.accounts[t.accountID]++
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
	accountID, _ := ctx.Value(bpsUploadAccountKey{}).(int64)
	t := &bpsUploadTicket{ready: make(chan struct{}), bytes: bytes, accountID: accountID}
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
	release := func() { s.mu.Lock(); s.release(t); s.dispatch(); s.mu.Unlock() }
	select {
	case <-ctx.Done():
		s.mu.Lock()
		if t.granted {
			s.release(t)
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
			v.AccountUploadLimit = bpsAccountUploadLimit()
		})
		return release, nil
	}
}

// Called under mu. Remove idle account counters so this map is bounded by
// active uploads, rather than the number of accounts ever seen by the process.
func (s *bpsUploadScheduler) release(t *bpsUploadTicket) {
	s.active--
	s.bytes -= t.bytes
	s.accounts[t.accountID]--
	if s.accounts[t.accountID] == 0 {
		delete(s.accounts, t.accountID)
	}
}
