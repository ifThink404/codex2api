package proxy

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
)

// Per-account BPS request budget, off by default (wlj-01 threshold
// experiment: usage-policy blocks start after ~1250-1360 successful BPS
// requests per account, regardless of tokens). bps_account_request_budget
// successful BPS upstream requests per account over bps_account_budget_window
// (default 24h), counted in 24 rolling buckets in the runtime cache so every
// replica sees them. An account at its budget is vetoed for BPS until old
// buckets roll out of the window.

const (
	BPSBudgetExhaustedReason = "bps_budget_exhausted"

	bpsBudgetNamespace = "bps-request-budget-v1"
	bpsBudgetBuckets   = 24
	bpsBudgetRefresh   = 5 * time.Second
)

var defaultBPSBudgetWindow = 24 * time.Hour

// BudgetWindow is bps_account_budget_window, default 24h.
func (c BPSConfig) BudgetWindow() time.Duration {
	if d, err := time.ParseDuration(c.AccountBudgetWindow); err == nil && d >= time.Hour {
		return d
	}
	return defaultBPSBudgetWindow
}

// bpsLocalBudgetCache backs budget counting when the handler has no runtime
// cache (process-local, like the in-memory cache driver).
var bpsLocalBudgetCache = cache.NewMemory(1)

type bpsBudgetEntry struct {
	used    int
	fetched time.Time
}

type bpsBudgetCounter struct {
	mu      sync.Mutex
	entries map[int64]bpsBudgetEntry
}

var bpsBudgets = &bpsBudgetCounter{}

func bpsBudgetStore(store cache.TokenCache) cache.TokenCache {
	if store = bpsGuardedCache(store); store != nil {
		return store
	}
	return bpsLocalBudgetCache
}

// bpsBudgetBucket is the bucket length and the current bucket index.
func bpsBudgetBucket(window time.Duration, now time.Time) (time.Duration, int64) {
	bucket := max(window/bpsBudgetBuckets, time.Minute)
	return bucket, now.UnixNano() / int64(bucket)
}

func bpsBudgetKey(accountID int64, window time.Duration, now time.Time) (key, previous string) {
	epoch := now.UnixNano() / int64(window)
	base := strconv.FormatInt(accountID, 10) + ":" + strconv.FormatInt(int64(window/time.Second), 10) + ":"
	return base + strconv.FormatInt(epoch, 10), base + strconv.FormatInt(epoch-1, 10)
}

// readBPSBudget sums the account's successful requests inside the window.
func readBPSBudget(ctx context.Context, store cache.TokenCache, accountID int64, window time.Duration, now time.Time) int {
	bucket, current := bpsBudgetBucket(window, now)
	oldest := current - int64(window/bucket) + 1
	key, previous := bpsBudgetKey(accountID, window, now)
	total := 0.0
	for _, k := range []string{key, previous} {
		readCtx, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
		counters, err := store.GetRuntimeCounters(readCtx, bpsBudgetNamespace, k)
		cancel()
		if err != nil {
			continue
		}
		for field, value := range counters {
			if index, err := strconv.ParseInt(field, 10, 64); err == nil && index >= oldest && index <= current {
				total += value
			}
		}
	}
	return int(math.Round(total))
}

// used is the account's count in the current window (cached briefly).
func (b *bpsBudgetCounter) used(ctx context.Context, store cache.TokenCache, accountID int64, window time.Duration) int {
	now := time.Now()
	b.mu.Lock()
	entry, ok := b.entries[accountID]
	b.mu.Unlock()
	if ok && now.Sub(entry.fetched) < bpsBudgetRefresh {
		return entry.used
	}
	used := readBPSBudget(ctx, bpsBudgetStore(store), accountID, window, now)
	b.mu.Lock()
	if b.entries == nil {
		b.entries = map[int64]bpsBudgetEntry{}
	}
	b.entries[accountID] = bpsBudgetEntry{used: used, fetched: now}
	b.mu.Unlock()
	return used
}

// record counts one successful BPS upstream request for the account.
func (b *bpsBudgetCounter) record(ctx context.Context, store cache.TokenCache, accountID int64, window time.Duration) {
	now := time.Now()
	_, index := bpsBudgetBucket(window, now)
	key, _ := bpsBudgetKey(accountID, window, now)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
	_ = bpsBudgetStore(store).IncrRuntimeCounters(writeCtx, bpsBudgetNamespace, key, map[string]float64{strconv.FormatInt(index, 10): 1}, 2*window)
	cancel()
	b.mu.Lock()
	if entry, ok := b.entries[accountID]; ok {
		entry.used++
		b.entries[accountID] = entry
	}
	b.mu.Unlock()
}

// bpsBudgetExhausted reports whether account used its request budget.
func bpsBudgetExhausted(ctx context.Context, store cache.TokenCache, account *auth.Account) bool {
	cfg := currentBPSConfig()
	if cfg.AccountRequestBudget <= 0 || account == nil {
		return false
	}
	return bpsBudgets.used(ctx, store, account.ID(), cfg.BudgetWindow()) >= cfg.AccountRequestBudget
}

// bpsBudgetUsage is the admin view of an account's budget and concurrency.
func bpsBudgetUsage(ctx context.Context, store cache.TokenCache, accountID int64) (used, budget, inflight, limit int) {
	cfg := currentBPSConfig()
	if cfg.AccountRequestBudget > 0 {
		used = bpsBudgets.used(ctx, store, accountID, cfg.BudgetWindow())
	}
	return used, cfg.AccountRequestBudget, bpsInflightRequests.current(accountID), cfg.AccountMaxConcurrency
}
