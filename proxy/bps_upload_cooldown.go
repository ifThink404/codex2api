package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	bpsUploadCooldownReason    = "bps_upload_cooldown"
	bpsUploadCooldownNamespace = "bps-upload-cooldown-v1"
	bpsUploadCooldownDefault   = time.Minute
	bpsUploadCooldownMax       = 5 * time.Minute
	bpsUploadCooldownLimit     = 4096
)

// Upload throttling is not inference quota exhaustion. Keep a bounded,
// account-scoped cooldown without changing the account's global health.
type bpsUploadCooldowns struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func bpsUploadCooldownKey(account *auth.Account) string {
	if account == nil {
		return ""
	}
	id := account.EffectiveAccountID()
	if id == "" {
		id = fmt.Sprintf("local:%d", account.ID())
	}
	return codexIdentityDigest(bpsUploadCooldownNamespace, id)
}

func (s *bpsUploadCooldowns) get(key string, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	until := s.entries[key]
	if !until.After(now) {
		delete(s.entries, key)
		return time.Time{}
	}
	return until
}

func (s *bpsUploadCooldowns) remember(key string, until, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior := s.entries[key]; prior.After(until) {
		return prior
	}
	if s.entries == nil {
		s.entries = make(map[string]time.Time)
	}
	if len(s.entries) >= bpsUploadCooldownLimit {
		for k, expiry := range s.entries {
			if !expiry.After(now) {
				delete(s.entries, k)
			}
		}
		if _, exists := s.entries[key]; !exists && len(s.entries) >= bpsUploadCooldownLimit {
			var oldestKey string
			var oldest time.Time
			for k, expiry := range s.entries {
				if oldestKey == "" || expiry.Before(oldest) {
					oldestKey, oldest = k, expiry
				}
			}
			delete(s.entries, oldestKey)
		}
	}
	s.entries[key] = until
	return until
}

func (h *Handler) readSharedBPSUploadCooldown(ctx context.Context, key string, now time.Time) (time.Time, error) {
	if h.cache == nil || !h.cache.SharedAcrossInstances() {
		return time.Time{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
	defer cancel()
	raw, found, err := h.cache.GetRuntime(ctx, bpsUploadCooldownNamespace, key)
	var until time.Time
	if err != nil {
		return time.Time{}, err
	}
	if !found || len(raw) > 128 || json.Unmarshal(raw, &until) != nil || !until.After(now) || until.After(now.Add(bpsUploadCooldownMax)) {
		return time.Time{}, nil
	}
	return until, nil
}

func (h *Handler) rememberBPSUploadFailure(ctx context.Context, account *auth.Account, failure error) {
	var upload *bpsAttachmentUploadError
	if h == nil || account == nil || !errors.As(failure, &upload) || upload.detail.Stage != "http" || upload.detail.HTTPStatus != http.StatusTooManyRequests || isHardStopUpstreamPolicyError(failure) || isExplicitUpstreamSafetyPolicy(upload.UpstreamErrorBody()) {
		return
	}
	now := time.Now()
	delay := parseRetryAfterHeaderAt(upload.detail.RetryAfter, now)
	if delay <= 0 {
		delay = bpsUploadCooldownDefault
	}
	delay = min(delay, bpsUploadCooldownMax)
	key := bpsUploadCooldownKey(account)
	until := h.bpsUploadCooldowns.remember(key, now.Add(delay), now)
	if h.cache == nil || !h.cache.SharedAcrossInstances() {
		return
	}
	// Serialize shared updates so an older/shorter failure cannot shorten a
	// newer cooldown. A cache outage still leaves the local observation active.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
	defer cancel()
	owner := NewUpstreamSessionUUID()
	locked, err := h.cache.AcquireLease(writeCtx, bpsUploadCooldownNamespace, key, owner, time.Second)
	if err != nil || !locked {
		return
	}
	defer func() {
		releaseCtx, done := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
		defer done()
		_ = h.cache.ReleaseLease(releaseCtx, bpsUploadCooldownNamespace, key, owner)
	}()
	shared, err := h.readSharedBPSUploadCooldown(writeCtx, key, now)
	if err != nil {
		return
	}
	if shared.After(until) {
		until = h.bpsUploadCooldowns.remember(key, shared, now)
	}
	raw, _ := json.Marshal(until)
	_ = h.cache.SetRuntime(writeCtx, bpsUploadCooldownNamespace, key, raw, time.Until(until))
}

type bpsUploadRequestKey struct{}

type bpsUploadRequest struct {
	handler        *Handler
	body           []byte
	parts          []gjson.Result
	headers        http.Header
	compact        bool
	mu             sync.Mutex
	sharedChecked  map[string]bool
	sharedDisabled bool
}

func (h *Handler) bindBPSUploadRequest(c *gin.Context, body []byte, compact bool) {
	// Text-only requests must not incur a shared-cache lookup per candidate.
	parts := bpsUploadParts(body, nil, compact, false)
	hasInline := false
	for _, part := range parts {
		kind := part.Get("type").String()
		url := part.Get("image_url").String()
		if kind == "image_url" {
			url = part.Get("image_url.url").String()
		}
		if len(url) >= 5 && strings.EqualFold(url[:5], "data:") || part.Get("file_data").Exists() || part.Get("file.file_data").Exists() || part.Get("source.type").String() == "base64" {
			hasInline = true
			break
		}
	}
	var s *bpsUploadRequest
	if hasInline {
		s = &bpsUploadRequest{handler: h, body: body, parts: parts, headers: c.Request.Header.Clone(), compact: compact, sharedChecked: make(map[string]bool)}
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), bpsUploadRequestKey{}, s))
}

// Only actual inline media carriers are visited. Text, tool schemas, arguments,
// opaque continuation data and remote URLs must never manufacture a cooldown.
func bpsUploadParts(body []byte, headers http.Header, compact, trim bool) []gjson.Result {
	root := gjson.ParseBytes(body)
	items := root.Get("input").Array()
	if trim && len(items) > 0 {
		raw := make([]json.RawMessage, len(items))
		for i, item := range items {
			raw[i] = json.RawMessage(item.Raw)
		}
		if trimmed, err := trimBPSImageHistory(raw, body, headers, compact, &CodexBPSDiagnostic{}); err == nil {
			for i, item := range trimmed {
				items[i] = gjson.ParseBytes(item)
			}
		}
	}
	if !root.Get("input").Exists() {
		items = root.Get("messages").Array()
	}
	var parts []gjson.Result
	var visit func(gjson.Result, int)
	visit = func(content gjson.Result, depth int) {
		if depth > 16 {
			return
		}
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "input_image", "image_url", "image", "input_file", "file", "document":
				parts = append(parts, part)
			case "tool_result": // Anthropic tool results may carry image blocks.
				visit(part.Get("content"), depth+1)
			}
		}
	}
	for _, item := range items {
		switch item.Get("type").String() {
		case "", "message":
			switch item.Get("role").String() {
			case "user", "assistant", "developer", "system", "tool":
				visit(item.Get("content"), 0)
			}
		case "function_call_output", "custom_tool_call_output":
			visit(item.Get("output"), 0)
		}
	}
	return parts
}

func bpsUploadPartKey(account *auth.Account, part gjson.Result) string {
	kind := part.Get("type").String()
	if part.Get("file_id").String() != "" && kind != "input_file" {
		return ""
	}
	if kind == "input_file" || kind == "file" || kind == "document" {
		file := part
		if kind == "file" {
			file = part.Get("file")
		}
		data, name := file.Get("file_data"), file.Get("filename").String()
		if kind == "document" && part.Get("source.type").String() == "base64" {
			data = gjson.Result{Type: gjson.String, Str: "data:" + part.Get("source.media_type").String() + ";base64," + part.Get("source.data").String()}
			name = part.Get("title").String()
		}
		if data.Type != gjson.String {
			return ""
		}
		decoded, err := decodeBPSFileData(data.String(), name)
		if err != nil {
			return ""
		} // Let normal input validation report it.
		return bpsFileUploadKey(account, decoded)
	}
	url := part.Get("image_url").String()
	if kind == "image_url" {
		url = part.Get("image_url.url").String()
	}
	if kind == "image" && part.Get("source.type").String() == "base64" {
		url = "data:" + part.Get("source.media_type").String() + ";base64," + part.Get("source.data").String()
	}
	if len(url) < 5 || !strings.EqualFold(url[:5], "data:") {
		return ""
	}
	detail := codexBPSImageDetail{}
	normalized := normalizeBPSImageDataURL(url, &detail)
	if detail.DetectedMIME == "" {
		return ""
	}
	_, encoded, _ := strings.Cut(normalized, ",")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	return bpsImageUploadKey(account, decoded)
}

func bpsUploadCooldownForRequest(ctx context.Context, account *auth.Account, mode string) bool {
	// Upload throttling with a local fallback is not an account health failure.
	// Keep this account usable; normal preparation will reuse IDs or adapt data.
	if account != nil && mode == "bps" && bpsAttachmentFallbackEnabled() {
		s := &bpsFallbackState{key: bpsAttachmentFallbackKey(account), registry: bpsFallbacks, backend: nil}
		if s.active(ctx) {
			return false
		}
	}
	// A temporary upload limit may only trigger account rotation when selected.
	// Keep observations so switching back to rotate retains the cooldown.
	if currentRateLimitRetryPolicy() != database.RateLimitRetryRotate {
		return false
	}
	s, _ := ctx.Value(bpsUploadRequestKey{}).(*bpsUploadRequest)
	if s == nil || account == nil || mode != "bps" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, now := bpsUploadCooldownKey(account), time.Now()
	until := s.handler.bpsUploadCooldowns.get(key, now)
	if !s.sharedDisabled && !s.sharedChecked[key] {
		s.sharedChecked[key] = true
		shared, err := s.handler.readSharedBPSUploadCooldown(ctx, key, now)
		if err != nil {
			s.sharedDisabled = true
		} else if shared.After(until) {
			until = s.handler.bpsUploadCooldowns.remember(key, shared, now)
		}
	}
	if !until.After(now) {
		return false
	}
	account.Mu().RLock()
	trim := account.CodexBPSImageTrim
	account.Mu().RUnlock()
	parts := s.parts
	if trim {
		parts = bpsUploadParts(s.body, s.headers, s.compact, true)
	}
	for _, part := range parts {
		attachmentKey := bpsUploadPartKey(account, part)
		if attachmentKey == "" {
			continue
		}
		bpsImages.mu.Lock()
		entry := bpsImages.entries[attachmentKey]
		ready := entry != nil && entry.err == nil && entry.id != "" && entry.expires.After(now)
		bpsImages.mu.Unlock()
		if ready {
			continue
		}
		if backend := bpsSharedAttachments(ctx); backend != nil {
			_, found, err := backend.read(ctx, attachmentKey)
			if !backend.failed(ctx, err) && found {
				continue
			}
		}
		if bpsAttachmentFallbackEnabled() {
			state := &bpsFallbackState{key: bpsAttachmentFallbackKey(account), registry: bpsFallbacks, backend: bpsSharedAttachments(ctx)}
			if state.active(ctx) {
				return false
			}
		}
		return true
	}
	return false
}
