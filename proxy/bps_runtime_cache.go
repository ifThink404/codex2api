package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"reflect"
	"sync"
	"time"

	"github.com/codex2api/cache"
)

// Runtime-cache backoff (adopted from upstream's official Excel BPS replay
// store). Every shared-cache call the BPS plugin makes (provenance markers,
// upload cooldowns, attachment handles and the attachment fallback window)
// goes through bpsRuntimeCache. After a failure the shared cache is skipped
// for bpsRuntimeCacheCooldown, so a slow or unavailable store cannot add its
// timeout to every request; callers already treat an error as "not shared"
// and keep their local state. Only the first failure of an outage is logged.

var bpsRuntimeCacheCooldown = 30 * time.Second

var errBPSRuntimeCacheDown = errors.New("BPS shared runtime cache is cooling down after a failure")

type bpsRuntimeBreaker struct {
	mu        sync.Mutex
	downUntil time.Time
}

// bpsRuntimeBreakers holds one breaker per underlying store, so an outage of
// one store never pauses another (tests use several).
var bpsRuntimeBreakers sync.Map

func bpsRuntimeBreakerFor(store cache.TokenCache) *bpsRuntimeBreaker {
	if !reflect.TypeOf(store).Comparable() {
		return &bpsRuntimeBreaker{}
	}
	breaker, _ := bpsRuntimeBreakers.LoadOrStore(store, &bpsRuntimeBreaker{})
	return breaker.(*bpsRuntimeBreaker)
}

func (b *bpsRuntimeBreaker) down() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.downUntil)
}

// observe trips the breaker on a store failure. A caller that cancelled its
// own request says nothing about the store.
func (b *bpsRuntimeBreaker) observe(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	b.mu.Lock()
	alreadyDown := time.Now().Before(b.downUntil)
	b.downUntil = time.Now().Add(bpsRuntimeCacheCooldown)
	b.mu.Unlock()
	if !alreadyDown {
		log.Printf("[bps] shared runtime cache unavailable, using local state only for %s: %v", bpsRuntimeCacheCooldown, err)
	}
	return err
}

// bpsRuntimeCache wraps the gateway cache for the BPS plugin's runtime calls.
type bpsRuntimeCache struct {
	cache.TokenCache
	breaker *bpsRuntimeBreaker
}

// bpsGuardedCache returns the plugin's view of store, or nil without one.
func bpsGuardedCache(store cache.TokenCache) cache.TokenCache {
	if store == nil {
		return nil
	}
	if _, ok := store.(*bpsRuntimeCache); ok {
		return store
	}
	return &bpsRuntimeCache{TokenCache: store, breaker: bpsRuntimeBreakerFor(store)}
}

func (h *Handler) bpsCache() cache.TokenCache {
	if h == nil {
		return nil
	}
	return bpsGuardedCache(h.cache)
}

func (c *bpsRuntimeCache) GetRuntime(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	if c.breaker.down() {
		return nil, false, errBPSRuntimeCacheDown
	}
	raw, found, err := c.TokenCache.GetRuntime(ctx, namespace, key)
	return raw, found, c.breaker.observe(err)
}

func (c *bpsRuntimeCache) SetRuntime(ctx context.Context, namespace, key string, value json.RawMessage, ttl time.Duration) error {
	if c.breaker.down() {
		return errBPSRuntimeCacheDown
	}
	return c.breaker.observe(c.TokenCache.SetRuntime(ctx, namespace, key, value, ttl))
}

func (c *bpsRuntimeCache) DeleteRuntime(ctx context.Context, namespace, key string) error {
	if c.breaker.down() {
		return errBPSRuntimeCacheDown
	}
	return c.breaker.observe(c.TokenCache.DeleteRuntime(ctx, namespace, key))
}

func (c *bpsRuntimeCache) AcquireLease(ctx context.Context, namespace, key, owner string, ttl time.Duration) (bool, error) {
	if c.breaker.down() {
		return false, errBPSRuntimeCacheDown
	}
	acquired, err := c.TokenCache.AcquireLease(ctx, namespace, key, owner, ttl)
	return acquired, c.breaker.observe(err)
}

// ReleaseLease always reaches the store: a lease taken before an outage must
// not be left to expire just because the breaker tripped meanwhile.
func (c *bpsRuntimeCache) ReleaseLease(ctx context.Context, namespace, key, owner string) error {
	return c.breaker.observe(c.TokenCache.ReleaseLease(ctx, namespace, key, owner))
}

func (c *bpsRuntimeCache) owner() (cache.RuntimeOwnerStore, error) {
	store, ok := c.TokenCache.(cache.RuntimeOwnerStore)
	if !ok {
		return nil, errors.New("runtime cache does not support owner records")
	}
	if c.breaker.down() {
		return nil, errBPSRuntimeCacheDown
	}
	return store, nil
}

func (c *bpsRuntimeCache) ClaimRuntimeOwner(ctx context.Context, namespace, key string, owner []byte, ttl time.Duration) ([]byte, error) {
	store, err := c.owner()
	if err != nil {
		return nil, err
	}
	current, err := store.ClaimRuntimeOwner(ctx, namespace, key, owner, ttl)
	return current, c.breaker.observe(err)
}

func (c *bpsRuntimeCache) CompareAndRefreshRuntimeOwner(ctx context.Context, namespace, key string, expected []byte, ttl time.Duration) (bool, error) {
	store, err := c.owner()
	if err != nil {
		return false, err
	}
	refreshed, err := store.CompareAndRefreshRuntimeOwner(ctx, namespace, key, expected, ttl)
	return refreshed, c.breaker.observe(err)
}

func (c *bpsRuntimeCache) CompareAndDeleteRuntimeOwner(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	store, err := c.owner()
	if err != nil {
		return false, err
	}
	deleted, err := store.CompareAndDeleteRuntimeOwner(ctx, namespace, key, expected)
	return deleted, c.breaker.observe(err)
}

// supportsRuntimeOwner reports whether the wrapped store has owner records.
func (c *bpsRuntimeCache) supportsRuntimeOwner() bool {
	_, ok := c.TokenCache.(cache.RuntimeOwnerStore)
	return ok
}
