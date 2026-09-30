package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/stretchr/testify/require"
)

// flakyRuntimeCache fails runtime calls while failing is set.
type flakyRuntimeCache struct {
	cache.TokenCache
	failing  atomic.Bool
	calls    atomic.Int32
	releases atomic.Int32
}

var errFlakyRuntimeCache = errors.New("runtime cache timeout")

func (c *flakyRuntimeCache) GetRuntime(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	c.calls.Add(1)
	if c.failing.Load() {
		return nil, false, errFlakyRuntimeCache
	}
	return c.TokenCache.GetRuntime(ctx, namespace, key)
}

func (c *flakyRuntimeCache) SetRuntime(ctx context.Context, namespace, key string, value json.RawMessage, ttl time.Duration) error {
	c.calls.Add(1)
	if c.failing.Load() {
		return errFlakyRuntimeCache
	}
	return c.TokenCache.SetRuntime(ctx, namespace, key, value, ttl)
}

func (c *flakyRuntimeCache) ReleaseLease(ctx context.Context, namespace, key, owner string) error {
	c.releases.Add(1)
	return c.TokenCache.ReleaseLease(ctx, namespace, key, owner)
}

func TestBPSRuntimeCacheBacksOffAfterAFailure(t *testing.T) {
	previous := bpsRuntimeCacheCooldown
	bpsRuntimeCacheCooldown = 50 * time.Millisecond
	t.Cleanup(func() { bpsRuntimeCacheCooldown = previous })
	store := &flakyRuntimeCache{TokenCache: cache.NewMemory(1)}
	guarded := bpsGuardedCache(store)
	require.Same(t, guarded, bpsGuardedCache(guarded), "wrapping is idempotent")

	require.NoError(t, guarded.SetRuntime(t.Context(), "ns", "k", json.RawMessage(`true`), time.Minute))
	store.failing.Store(true)
	_, _, err := guarded.GetRuntime(t.Context(), "ns", "k")
	require.ErrorIs(t, err, errFlakyRuntimeCache)
	calls := store.calls.Load()

	// While cooling down, calls skip the store entirely.
	_, _, err = guarded.GetRuntime(t.Context(), "ns", "k")
	require.ErrorIs(t, err, errBPSRuntimeCacheDown)
	require.ErrorIs(t, guarded.SetRuntime(t.Context(), "ns", "k", json.RawMessage(`true`), time.Minute), errBPSRuntimeCacheDown)
	require.Equal(t, calls, store.calls.Load())
	// A lease taken before the outage is still released.
	require.NoError(t, guarded.ReleaseLease(t.Context(), "ns", "k", "owner"))
	require.EqualValues(t, 1, store.releases.Load())
	// Another store keeps working.
	other := bpsGuardedCache(cache.NewMemory(1))
	require.NoError(t, other.SetRuntime(t.Context(), "ns", "k", json.RawMessage(`true`), time.Minute))

	store.failing.Store(false)
	require.Eventually(t, func() bool {
		_, found, err := guarded.GetRuntime(t.Context(), "ns", "k")
		return err == nil && found
	}, time.Second, 10*time.Millisecond, "the store is used again after the cooldown")
}

func TestBPSRuntimeCacheIgnoresCallerCancellation(t *testing.T) {
	store := &flakyRuntimeCache{TokenCache: cache.NewMemory(1)}
	guarded := bpsGuardedCache(store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	breaker := guarded.(*bpsRuntimeCache).breaker
	require.ErrorIs(t, breaker.observe(ctx.Err()), context.Canceled)
	require.False(t, breaker.down(), "a cancelled caller says nothing about the store")
	_, _, err := guarded.GetRuntime(t.Context(), "ns", "missing")
	require.NoError(t, err)
}

func TestBPSRuntimeCacheOwnerRecordsFollowTheStore(t *testing.T) {
	memory := cache.NewMemory(1)
	_, memoryOwner := memory.(cache.RuntimeOwnerStore)
	require.Equal(t, memoryOwner, bpsGuardedCache(memory).(*bpsRuntimeCache).supportsRuntimeOwner())

	// A store without owner records stays without them behind the wrapper.
	plain := bpsGuardedCache(&flakyRuntimeCache{TokenCache: memory}).(*bpsRuntimeCache)
	require.False(t, plain.supportsRuntimeOwner())
	_, err := plain.CompareAndDeleteRuntimeOwner(t.Context(), "ns", "k", nil)
	require.Error(t, err)
}
