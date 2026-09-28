package proxy

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/cache"
)

const bpsAttachmentNamespace = "bps-attachment-v1"
const bpsAttachmentCacheTimeout = 250 * time.Millisecond
const bpsAttachmentLeaseTTL = 65 * time.Second

type bpsAttachmentBackendKey struct{}
type bpsAttachmentBackend struct {
	store    cache.TokenCache
	disabled atomic.Bool
}
type bpsAttachmentRecord struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Expires time.Time `json:"expires"`
}

// Only hashed account/content keys and opaque attachment handles are shared.
// Request-scoped circuit breaking keeps a cache outage from delaying every image.
func WithBPSAttachmentCache(ctx context.Context, store cache.TokenCache) context.Context {
	if store == nil || !store.SharedAcrossInstances() {
		return ctx
	}
	if guarded, ok := store.(*bpsRuntimeCache); ok && !guarded.supportsRuntimeOwner() {
		return ctx
	}
	if _, ok := store.(cache.RuntimeOwnerStore); !ok {
		return ctx
	}
	return context.WithValue(ctx, bpsAttachmentBackendKey{}, &bpsAttachmentBackend{store: store})
}

func bpsSharedAttachments(ctx context.Context) *bpsAttachmentBackend {
	b, _ := ctx.Value(bpsAttachmentBackendKey{}).(*bpsAttachmentBackend)
	if b == nil || b.disabled.Load() {
		return nil
	}
	return b
}

func (b *bpsAttachmentBackend) failed(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	b.disabled.Store(true)
	bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.SharedCacheErrors++ })
	return true
}

func bpsAttachmentTTL() time.Duration {
	// Keep the known 30-minute policy unless an operator has validated a longer
	// upstream lifetime. An explicit missing-file response always invalidates it.
	minutes, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CODEX_BPS_ATTACHMENT_CACHE_TTL_MINUTES")))
	if err == nil && minutes >= 1 && minutes <= 1440 {
		return time.Duration(minutes) * time.Minute
	}
	return bpsAttachmentCacheTTL
}

func validBPSAttachmentID(id string) bool {
	return strings.HasPrefix(id, "file-") && len(id) > len("file-") && len(id) <= 256 && !strings.ContainsAny(id, " \t\r\n/\\")
}

func (b *bpsAttachmentBackend) read(ctx context.Context, key string) (bpsAttachmentRecord, bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
	defer cancel()
	raw, found, err := b.store.GetRuntime(readCtx, bpsAttachmentNamespace, key)
	var record bpsAttachmentRecord
	if err != nil || !found {
		return record, false, err
	}
	if len(raw) > 1024 || json.Unmarshal(raw, &record) != nil || record.Version != 1 || !validBPSAttachmentID(record.ID) || !record.Expires.After(time.Now()) || record.Expires.After(time.Now().Add(24*time.Hour+time.Minute)) {
		_, err = b.store.(cache.RuntimeOwnerStore).CompareAndDeleteRuntimeOwner(readCtx, bpsAttachmentNamespace, key, raw)
		return bpsAttachmentRecord{}, false, err
	}
	return record, true, nil
}

func resolveSharedBPSAttachment(ctx context.Context, key string, upload func() (string, error)) (string, time.Time, bool, error) {
	var waitStart time.Time
	stopWaiting := func() {
		if !waitStart.IsZero() {
			bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.SharedCacheWaitMS += time.Since(waitStart).Milliseconds() })
			waitStart = time.Time{}
		}
	}
	defer stopWaiting()
	uploadOnly := func() (string, time.Time, bool, error) {
		stopWaiting()
		if err := ctx.Err(); err != nil {
			return "", time.Time{}, false, err
		}
		id, err := upload()
		return id, time.Now().Add(bpsAttachmentTTL()), false, err
	}
	b := bpsSharedAttachments(ctx)
	if b == nil {
		return uploadOnly()
	}
	timing := bpsTimingFromContext(ctx)
	owner := NewUpstreamSessionUUID()
	for {
		record, found, err := b.read(ctx, key)
		if b.failed(ctx, err) {
			return uploadOnly()
		}
		if found {
			timing.update(func(v *bpsTimingValues) { v.SharedCacheHits++ })
			return record.ID, record.Expires, true, nil
		}
		leaseCtx, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
		acquired, err := b.store.AcquireLease(leaseCtx, bpsAttachmentNamespace, key, owner, bpsAttachmentLeaseTTL)
		cancel()
		if b.failed(ctx, err) {
			return uploadOnly()
		}
		if acquired {
			stopWaiting()
			defer func() {
				releaseCtx, done := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
				defer done()
				b.failed(ctx, b.store.ReleaseLease(releaseCtx, bpsAttachmentNamespace, key, owner))
			}()
			// A publisher can finish between the first lookup and lease acquire.
			record, found, err = b.read(ctx, key)
			if b.failed(ctx, err) {
				return uploadOnly()
			}
			if found {
				timing.update(func(v *bpsTimingValues) { v.SharedCacheHits++ })
				return record.ID, record.Expires, true, nil
			}
			id, expires, _, err := uploadOnly()
			if err == nil && validBPSAttachmentID(id) {
				raw, _ := json.Marshal(bpsAttachmentRecord{Version: 1, ID: id, Expires: expires})
				writeCtx, done := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
				b.failed(ctx, b.store.SetRuntime(writeCtx, bpsAttachmentNamespace, key, raw, time.Until(expires)))
				done()
			}
			return id, expires, false, err
		}
		if waitStart.IsZero() {
			waitStart = time.Now()
			timing.update(func(v *bpsTimingValues) { v.SharedCacheWaits++ })
		}
		if time.Since(waitStart) >= bpsAttachmentLeaseTTL {
			return uploadOnly()
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", time.Time{}, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func forgetSharedBPSAttachment(ctx context.Context, key, id string) {
	b := bpsSharedAttachments(ctx)
	if b == nil {
		return
	}
	lookup, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
	defer cancel()
	raw, found, err := b.store.GetRuntime(lookup, bpsAttachmentNamespace, key)
	if b.failed(ctx, err) || !found {
		return
	}
	var record bpsAttachmentRecord
	if len(raw) > 1024 || json.Unmarshal(raw, &record) != nil || record.ID != id {
		return
	}
	_, err = b.store.(cache.RuntimeOwnerStore).CompareAndDeleteRuntimeOwner(lookup, bpsAttachmentNamespace, key, raw)
	b.failed(ctx, err)
}
