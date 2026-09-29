package proxy

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/codex2api/auth"
)

// Per-account BPS concurrency cap, off by default (wlj-01 threshold
// experiment: usage-policy blocks come much earlier at high concurrency, and
// a per-account 429 appears within ~1.5 min at concurrency >= 20).
// bps_account_max_concurrency allows at most N BPS requests in flight per
// account on this replica. A full account is vetoed so the request goes to
// another BPS account; when none is free the attempt waits briefly for a
// slot, then fails with a clear 429.

const BPSConcurrencyFullReason = "bps_concurrency_full"

// bpsConcurrencyWait is how long an attempt admitted as the last resort
// waits for a free slot on a full account (a variable for tests).
var bpsConcurrencyWait = 10 * time.Second

// bpsInflight counts BPS requests in flight per account on this replica.
type bpsInflight struct {
	mu      sync.Mutex
	count   map[int64]int
	changed chan struct{}
}

var bpsInflightRequests = &bpsInflight{}

func (b *bpsInflight) current(accountID int64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count[accountID]
}

// tryAcquire takes a slot when fewer than limit are in flight.
func (b *bpsInflight) tryAcquire(accountID int64, limit int) (release func(), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count == nil {
		b.count = map[int64]int{}
	}
	if limit > 0 && b.count[accountID] >= limit {
		return nil, false
	}
	b.count[accountID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.count[accountID]--
			if b.count[accountID] <= 0 {
				delete(b.count, accountID)
			}
			if b.changed != nil {
				close(b.changed)
				b.changed = nil
			}
			b.mu.Unlock()
		})
	}, true
}

// acquire waits up to wait for a slot.
func (b *bpsInflight) acquire(ctx context.Context, accountID int64, limit int, wait time.Duration) (func(), bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		if release, ok := b.tryAcquire(accountID, limit); ok {
			return release, true
		}
		b.mu.Lock()
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline.C:
			return nil, false
		case <-changed:
		}
	}
}

// bpsConcurrencyFull reports whether account is at its BPS concurrency cap.
func bpsConcurrencyFull(account *auth.Account) bool {
	limit := currentBPSConfig().AccountMaxConcurrency
	return limit > 0 && account != nil && bpsInflightRequests.current(account.ID()) >= limit
}

// bpsReleasingBody releases the concurrency slot once the body ends or closes.
type bpsReleasingBody struct {
	io.ReadCloser
	release func()
}

func (b *bpsReleasingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *bpsReleasingBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}

// bpsLastRequest remembers when each account last started a BPS request on
// this replica.
var bpsLastRequest sync.Map

func bpsNoteRequest(accountID int64, at time.Time) { bpsLastRequest.Store(accountID, at) }

// bpsLastRequestAt is the account's last BPS request start on this replica.
func bpsLastRequestAt(accountID int64) time.Time {
	if at, ok := bpsLastRequest.Load(accountID); ok {
		return at.(time.Time)
	}
	return time.Time{}
}
