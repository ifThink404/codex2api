package proxy

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Successes and in-flight work have separate bounds. Failed uploads never evict
// successful handles. LRU operations are constant time, including cache hits.
const bpsAttachmentCacheLimit = 4096
const bpsAttachmentCacheTTL = 30 * time.Minute

type bpsAttachmentEntry struct {
	id      string
	err     error
	expires time.Time
	key     string
	element *list.Element
	bytes   int64
}

type bpsAttachmentFlight struct {
	ready   chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	waiters int
	id      string
	err     error
	shared  bool
}

type bpsAttachmentCache struct {
	mu      sync.Mutex
	entries map[string]*bpsAttachmentEntry
	lru     list.List
	bytes   int64
	flights map[string]*bpsAttachmentFlight
	changed chan struct{}
}

var bpsImages bpsAttachmentCache

func (c *bpsAttachmentCache) remove(e *bpsAttachmentEntry) {
	delete(c.entries, e.key)
	if e.element != nil {
		c.lru.Remove(e.element)
	}
	c.bytes -= e.bytes
}

// Kept for small callers that don't need a cancellation-aware uploader.
func (c *bpsAttachmentCache) resolve(ctx context.Context, key string, upload func() (string, error)) (string, bool, error) {
	return c.resolveContext(ctx, key, func(context.Context) (string, error) { return upload() })
}

func (c *bpsAttachmentCache) resolveContext(ctx context.Context, key string, upload func(context.Context) (string, error)) (string, bool, error) {
	timing := bpsTimingFromContext(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[string]*bpsAttachmentEntry)
		}
		if c.flights == nil {
			c.flights = make(map[string]*bpsAttachmentFlight)
		}
		if c.changed == nil {
			c.changed = make(chan struct{})
		}
		limit := bpsLocalAttachmentLimit()
		timing.update(func(v *bpsTimingValues) {
			v.CacheEntryLimit = limit
			v.UploadConcurrencyLimit = bpsRequestUploadLimit()
		})
		if e := c.entries[key]; e != nil {
			if time.Now().Before(e.expires) {
				c.lru.MoveToFront(e.element)
				id := e.id
				c.mu.Unlock()
				timing.update(func(v *bpsTimingValues) { v.CacheHits++ })
				return id, true, nil
			}
			c.remove(e)
			timing.update(func(v *bpsTimingValues) { v.CacheExpiredEntries++ })
		}
		flight := c.flights[key]
		if flight != nil && flight.ctx.Err() != nil {
			// The last waiter canceled it. Wait for cleanup before starting anew.
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-flight.ready:
				continue
			}
		}
		leader := flight == nil
		if leader {
			// Bound even uploads waiting for the instance scheduler or a shared
			// lease. Do not bypass the bound with an untracked network call.
			if len(c.flights) >= 4096 {
				changed := c.changed
				c.mu.Unlock()
				select {
				case <-ctx.Done():
					return "", false, ctx.Err()
				case <-changed:
					continue
				}
			}
			workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
			flight = &bpsAttachmentFlight{ready: make(chan struct{}), ctx: workCtx, cancel: cancel}
			c.flights[key] = flight
			timing.update(func(v *bpsTimingValues) {
				v.CacheMisses++
				v.CacheEntriesAtMissMax = max(v.CacheEntriesAtMissMax, len(c.entries))
			})
		} else {
			timing.update(func(v *bpsTimingValues) { v.CacheWaits++ })
		}
		flight.waiters++
		c.mu.Unlock()
		if leader {
			go c.complete(key, flight, upload, timing)
		}
		started := time.Now()
		select {
		case <-ctx.Done():
			c.mu.Lock()
			flight.waiters--
			last := flight.waiters == 0
			if last {
				flight.cancel()
			}
			c.mu.Unlock()
			// No detached request-owned upload remains after the final waiter.
			if last {
				<-flight.ready
			}
			return "", false, ctx.Err()
		case <-flight.ready:
			c.mu.Lock()
			flight.waiters--
			c.mu.Unlock()
			if !leader {
				timing.update(func(v *bpsTimingValues) { v.CacheWaitMS += time.Since(started).Milliseconds() })
			}
			return flight.id, (!leader || flight.shared) && flight.err == nil, flight.err
		}
	}
}

func (c *bpsAttachmentCache) complete(key string, f *bpsAttachmentFlight, upload func(context.Context) (string, error), timing *bpsTimingDiagnostic) {
	defer f.cancel()
	id, expires, shared, err := resolveSharedBPSAttachment(f.ctx, key, func() (string, error) { return upload(f.ctx) })
	c.mu.Lock()
	defer c.mu.Unlock()
	f.id, f.err, f.shared = id, err, shared
	if shared {
		timing.update(func(v *bpsTimingValues) { v.CacheMisses--; v.CacheHits++ })
	}
	if err == nil && validBPSAttachmentID(id) {
		limit, budget := bpsLocalAttachmentLimit(), bpsLocalAttachmentBytes()
		// Include a conservative allowance for map/list/node allocation.
		e := &bpsAttachmentEntry{id: id, expires: expires, key: key, bytes: int64(len(key) + len(id) + 256)}
		if e.bytes <= budget {
			for len(c.entries) >= limit || c.bytes+e.bytes > budget {
				old := c.lru.Back()
				if old == nil {
					break
				}
				c.remove(old.Value.(*bpsAttachmentEntry))
				timing.update(func(v *bpsTimingValues) { v.CacheEvictions++ })
			}
			e.element = c.lru.PushFront(e)
			c.entries[key] = e
			c.bytes += e.bytes
		}
	}
	delete(c.flights, key)
	close(f.ready)
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *bpsAttachmentCache) forget(key, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil && e.id == id {
		c.remove(e)
	}
}
