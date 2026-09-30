package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
)

const bpsAttachmentFallbackWindow = 2 * time.Hour
const bpsAttachmentFallbackNamespace = "bps-attachment-fallback-v1"

var errBPSAttachmentFallback = errors.New("use attachment fallback carrier")

type bpsFallbackEntry struct {
	until   time.Time
	probing bool
}

type bpsFallbackRegistry struct {
	mu      sync.Mutex
	entries map[string]*bpsFallbackEntry
}

var bpsFallbacks = &bpsFallbackRegistry{entries: make(map[string]*bpsFallbackEntry)}

type bpsFallbackContextKey struct{}
type bpsFallbackState struct {
	key      string
	registry *bpsFallbackRegistry
	backend  *bpsAttachmentBackend
	once     sync.Once
}

// The local 429 fallback converts attachments with pdftotext/libreoffice. It
// is OFF unless enabled in the BPS plugin config (or CODEX_BPS_ATTACHMENT_429_FALLBACK
// = on/true/1), and never active when neither converter binary is installed.
func bpsAttachmentFallbackEnabled() bool {
	enabled := currentBPSConfig().Attachment429Fallback
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_BPS_ATTACHMENT_429_FALLBACK"))) {
	case "on", "true", "1":
		enabled = true
	case "off", "false", "0":
		enabled = false
	}
	return enabled && bpsAttachmentConvertersInstalled()
}

var bpsConverterProbe struct {
	once      sync.Once
	installed bool
}

func bpsAttachmentConvertersInstalled() bool {
	bpsConverterProbe.once.Do(func() {
		for _, name := range []string{"pdftotext", "libreoffice"} {
			if _, err := exec.LookPath(name); err == nil {
				bpsConverterProbe.installed = true
			}
		}
	})
	return bpsConverterProbe.installed
}

func bpsAttachmentFallbackKey(a *auth.Account) string {
	return codexIdentityDigest(bpsAttachmentFallbackNamespace, fmt.Sprintf("%d:%s", a.ID(), a.EffectiveAccountID()))
}

func withBPSAttachmentFallback(ctx context.Context, a *auth.Account) context.Context {
	if !bpsAttachmentFallbackEnabled() {
		return ctx
	}
	return context.WithValue(ctx, bpsFallbackContextKey{}, &bpsFallbackState{key: bpsAttachmentFallbackKey(a), registry: bpsFallbacks, backend: bpsSharedAttachments(ctx)})
}

func bpsFallbackFromContext(ctx context.Context) *bpsFallbackState {
	s, _ := ctx.Value(bpsFallbackContextKey{}).(*bpsFallbackState)
	return s
}

func (s *bpsFallbackState) read(ctx context.Context) (time.Time, []byte, error) {
	if s.backend == nil {
		return time.Time{}, nil, nil
	}
	c, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
	defer cancel()
	raw, found, err := s.backend.store.GetRuntime(c, bpsAttachmentFallbackNamespace, s.key)
	var until time.Time
	if err != nil || !found {
		return time.Time{}, nil, err
	}
	if len(raw) > 128 || json.Unmarshal(raw, &until) != nil || until.After(time.Now().Add(bpsAttachmentFallbackWindow+time.Minute)) {
		return time.Time{}, nil, nil
	}
	return until, raw, nil
}

func (s *bpsFallbackState) remember(until time.Time) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if e := s.registry.entries[s.key]; e != nil {
		if until.After(e.until) {
			e.until = until
		}
		return
	}
	if len(s.registry.entries) >= 4096 {
		for k, e := range s.registry.entries {
			if !e.probing && time.Since(e.until) > 3*time.Minute {
				delete(s.registry.entries, k)
			}
		}
		// Never evict active cooldowns to admit a healthy account.
		if len(s.registry.entries) >= 4096 {
			return
		}
	}
	s.registry.entries[s.key] = &bpsFallbackEntry{until: until}
}

func (s *bpsFallbackState) load(ctx context.Context) {
	s.once.Do(func() {
		if until, _, err := s.read(ctx); err == nil && !until.IsZero() {
			s.remember(until)
		}
	})
}

func (s *bpsFallbackState) active(ctx context.Context) bool {
	if s == nil {
		return false
	}
	s.load(ctx)
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	e := s.registry.entries[s.key]
	if e == nil {
		return false
	}
	if e.until.After(time.Now()) {
		bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.AttachmentFallbackUntilMS = e.until.UnixMilli() })
		bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.AttachmentFallbackReason = "upload_rate_limited" })
		return true
	}
	return false
}

func (s *bpsFallbackState) trip(ctx context.Context) {
	until := time.Now().Add(bpsAttachmentFallbackWindow)
	s.remember(until)
	bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) {
		v.AttachmentFallbackUntilMS = until.UnixMilli()
		v.AttachmentFallbackTransitions++
		v.AttachmentFallbackReason = "upload_rate_limited"
	})
	if s.backend == nil {
		return
	}
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
	defer cancel()
	owner := NewUpstreamSessionUUID()
	locked, err := s.backend.store.AcquireLease(c, bpsAttachmentFallbackNamespace, s.key, owner, time.Second)
	if err != nil || !locked {
		return
	}
	defer func() {
		release, done := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
		defer done()
		_ = s.backend.store.ReleaseLease(release, bpsAttachmentFallbackNamespace, s.key, owner)
	}()
	prior, _, err := s.read(c)
	if err != nil {
		return
	}
	if prior.After(until) {
		until = prior
		s.remember(until)
	}
	raw, _ := json.Marshal(until)
	// Keep the expired record briefly so a restart still performs one recovery probe.
	_ = s.backend.store.SetRuntime(c, bpsAttachmentFallbackNamespace, s.key, raw, time.Until(until)+3*time.Minute)
}

func bpsUpload429(err error) bool {
	var e *bpsAttachmentUploadError
	if !errors.As(err, &e) || e.detail.Stage != "http" || e.detail.HTTPStatus != http.StatusTooManyRequests || isHardStopUpstreamPolicyError(err) || isExplicitUpstreamSafetyPolicy(e.UpstreamErrorBody()) {
		return false
	}
	// Do not turn every 429 into a carrier change. This type originates only
	// from /attachments; additionally require an explicit upload rate-limit
	// code or the exact message observed on that endpoint in production.
	code := strings.ToLower(strings.TrimSpace(e.detail.Code))
	switch strings.ToLower(strings.TrimSpace(e.detail.Type)) {
	case "", "server_error", "rate_limit_error":
	default:
		return false
	}
	message := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(e.detail.Message)), ".")
	switch code {
	case "upload_rate_limit_exceeded", "attachment_rate_limit_exceeded":
		return true
	case "", "rate_limit_exceeded":
		switch message {
		case "429: rate limit exceeded", "rate limit exceeded", "upload rate limit exceeded", "attachment upload rate limit exceeded":
			return true
		}
	}
	return false
}

// Cache lookups run before this function. A cooldown therefore never prevents
// reusing a valid file_id, and no fallback marker is stored as a file handle.
func bpsUploadWithFallback(ctx context.Context, upload func(context.Context) (string, error)) (string, error) {
	s := bpsFallbackFromContext(ctx)
	if s == nil {
		return upload(ctx)
	}
	if s.active(ctx) {
		return "", errBPSAttachmentFallback
	}
	var probeUntil time.Time
	s.registry.mu.Lock()
	if e := s.registry.entries[s.key]; e != nil {
		if e.probing {
			s.registry.mu.Unlock()
			bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.AttachmentFallbackReason = "recovery_probe_in_progress" })
			return "", errBPSAttachmentFallback
		}
		e.probing = true
		probeUntil = e.until
	}
	s.registry.mu.Unlock()
	var sharedRaw []byte
	if !probeUntil.IsZero() {
		defer func() {
			s.registry.mu.Lock()
			if e := s.registry.entries[s.key]; e != nil {
				e.probing = false
			}
			s.registry.mu.Unlock()
		}()
		if s.backend != nil {
			owner := NewUpstreamSessionUUID()
			c, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
			ok, err := s.backend.store.AcquireLease(c, bpsAttachmentFallbackNamespace+"-probe", s.key, owner, 3*time.Minute)
			cancel()
			if err == nil && !ok {
				bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.AttachmentFallbackReason = "recovery_probe_in_progress" })
				return "", errBPSAttachmentFallback
			}
			if ok {
				defer func() {
					c, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
					defer cancel()
					_ = s.backend.store.ReleaseLease(c, bpsAttachmentFallbackNamespace+"-probe", s.key, owner)
				}()
			}
			until, raw, err := s.read(ctx)
			sharedRaw = raw
			if err == nil && until.After(time.Now()) {
				s.remember(until)
				return "", errBPSAttachmentFallback
			}
		}
		bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.AttachmentRecoveryProbes++ })
	}
	id, err := upload(ctx)
	if bpsUpload429(err) {
		s.trip(ctx)
		return "", errBPSAttachmentFallback
	}
	if !probeUntil.IsZero() && err == nil {
		s.registry.mu.Lock()
		if e := s.registry.entries[s.key]; e != nil && e.until.Equal(probeUntil) {
			delete(s.registry.entries, s.key)
		}
		s.registry.mu.Unlock()
		if s.backend != nil && len(sharedRaw) > 0 {
			if owner, ok := s.backend.store.(cache.RuntimeOwnerStore); ok {
				c, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
				_, _ = owner.CompareAndDeleteRuntimeOwner(c, bpsAttachmentFallbackNamespace, s.key, sharedRaw)
				cancel()
			}
		}
	}
	return id, err
}
