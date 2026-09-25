package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Only opaque handles are cached, never credentials, original filenames or
// media bytes. The account is part of the key: handles cannot cross accounts.
const bpsAttachmentCacheLimit = 256
const bpsAttachmentCacheTTL = 30 * time.Minute

type bpsAttachmentEntry struct {
	ready   chan struct{}
	id      string
	err     error
	expires time.Time
}
type bpsAttachmentCache struct {
	mu      sync.Mutex
	entries map[string]*bpsAttachmentEntry
}

var bpsImages = bpsAttachmentCache{entries: make(map[string]*bpsAttachmentEntry)}

func (c *bpsAttachmentCache) resolve(ctx context.Context, key string, upload func() (string, error)) (string, bool, error) {
	timing := bpsTimingFromContext(ctx)
	c.mu.Lock()
	now := time.Now()
	expired := 0
	for k, e := range c.entries {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			delete(c.entries, k)
			expired++
		}
	}
	timing.update(func(v *bpsTimingValues) {
		v.CacheExpiredEntries += expired
		v.CacheEntryLimit = bpsAttachmentCacheLimit
		v.UploadConcurrencyLimit = bpsAttachmentUploadConcurrency
	})
	if e := c.entries[key]; e != nil {
		waiting := e.expires.IsZero()
		c.mu.Unlock()
		if waiting {
			started := time.Now()
			timing.update(func(v *bpsTimingValues) { v.CacheWaits++ })
			defer func() {
				timing.update(func(v *bpsTimingValues) { v.CacheWaitMS += time.Since(started).Milliseconds() })
			}()
		} else {
			timing.update(func(v *bpsTimingValues) { v.CacheHits++ })
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-e.ready:
			return e.id, e.err == nil, e.err
		}
	}
	timing.update(func(v *bpsTimingValues) {
		v.CacheMisses++
		v.CacheEntriesAtMissMax = max(v.CacheEntriesAtMissMax, len(c.entries))
	})
	if len(c.entries) >= bpsAttachmentCacheLimit {
		oldestKey := ""
		var oldest time.Time
		for k, e := range c.entries {
			if !e.expires.IsZero() && (oldestKey == "" || e.expires.Before(oldest)) {
				oldestKey, oldest = k, e.expires
			}
		}
		if oldestKey != "" {
			delete(c.entries, oldestKey)
			timing.update(func(v *bpsTimingValues) { v.CacheEvictions++ })
		} else {
			timing.update(func(v *bpsTimingValues) { v.CacheCapacityBypasses++ })
			// Do not let many concurrent unique uploads grow the cache unbounded.
			c.mu.Unlock()
			id, err := upload()
			return id, false, err
		}
	}
	e := &bpsAttachmentEntry{ready: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()
	id, expires, shared, err := resolveSharedBPSAttachment(ctx, key, upload)
	c.mu.Lock()
	e.id, e.err, e.expires = id, err, expires
	if shared {
		timing.update(func(v *bpsTimingValues) { v.CacheMisses--; v.CacheHits++ })
	}
	if err != nil {
		delete(c.entries, key)
	}
	close(e.ready)
	c.mu.Unlock()
	return id, shared, err
}

func (c *bpsAttachmentCache) forget(key, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil && !e.expires.IsZero() && e.id == id {
		delete(c.entries, key)
	}
}

func bpsImageUploadKey(account *auth.Account, data []byte) string {
	digest := sha256.Sum256(data)
	return codexIdentityDigest("bps-image-upload-v1", fmt.Sprintf("%d:%s", account.ID(), account.EffectiveAccountID()), hex.EncodeToString(digest[:]))
}

// Upload data-URL images from user messages and both tool-result carriers.
// Tool references are moved to labelled attachment messages in the next pass.
// Keep the same message, content order, detail and all other fields. Never
// manufacture assistant tool calls or fetch arbitrary remote image URLs.
func prepareBPSUserImageAttachments(ctx context.Context, account *auth.Account, body []byte, d *CodexBPSDiagnostic, upload func(context.Context, []byte, string) (string, error)) ([]byte, map[string]string, error) {
	used := make(map[string]string)
	type imageJob struct {
		url, diagnosticPath, adapted string
		itemIndex, partIndex         int
		field                        string
		key, id                      string
		reused                       bool
	}
	var jobs []imageJob
	if d != nil && d.Images != nil {
		d.Images.Uploaded, d.Images.UploadReused, d.Images.ToolAttachmentMessages = 0, 0, 0
	}
	for i, item := range gjson.GetBytes(body, "input").Array() {
		field, adapted := "", ""
		switch item.Get("type").String() {
		case "message", "":
			if item.Get("role").String() == "user" {
				field, adapted = "content", "user input image → uploaded attachment"
			}
		case "function_call_output", "custom_tool_call_output":
			field, adapted = "output", "tool input image → uploaded attachment"
		}
		if field == "" {
			continue
		}
		for j, part := range item.Get(field).Array() {
			if part.Get("type").String() != "input_image" || part.Get("file_id").String() != "" {
				continue
			}
			url := part.Get("image_url").String()
			if len(url) < 5 || !strings.EqualFold(url[:5], "data:") {
				continue
			}
			if field == "output" && strings.TrimSpace(item.Get("call_id").String()) == "" {
				return nil, nil, bpsImageInputError("图片工具结果缺少 call_id，无法关联原工具调用。")
			}
			jobs = append(jobs, imageJob{url: url, itemIndex: i, partIndex: j, field: field, diagnosticPath: fmt.Sprintf("input[%d].%s[%d]", i, field, j), adapted: adapted})
		}
	}
	err := runBPSAttachmentJobs(ctx, len(jobs), func(workCtx context.Context, index int) error {
		job := &jobs[index]
		detail := codexBPSImageDetail{}
		normalized := normalizeBPSImageDataURL(job.url, &detail)
		if detail.DetectedMIME == "" {
			return bpsImageInputError("图片数据无效或格式不受支持，请使用 PNG、JPEG、GIF 或 WebP 图片。")
		}
		_, encoded, _ := strings.Cut(normalized, ",")
		if len(encoded) > security.MaxRequestBodySize {
			return bpsImageInputError("图片数据超过请求大小限制。")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return bpsImageInputError("图片 Base64 数据不完整或无效，请重新附加图片。")
		}
		job.key = bpsImageUploadKey(account, data)
		job.id, job.reused, err = bpsImages.resolve(workCtx, job.key, func() (string, error) {
			return upload(workCtx, data, detail.DetectedMIME)
		})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	references := make(map[int][]bpsImageReference)
	for _, job := range jobs {
		references[job.itemIndex] = append(references[job.itemIndex], bpsImageReference{Part: job.partIndex, Field: job.field, ID: job.id})
	}
	body, err = rewriteBPSImageReferences(body, references)
	if err != nil {
		return nil, nil, ErrInternalError("构建图片附件引用失败", err)
	}
	for _, job := range jobs {
		used[job.key] = job.id
		if d != nil && d.Images != nil {
			if job.reused {
				d.Images.UploadReused++
			} else {
				d.Images.Uploaded++
			}
			for k := range d.Images.Details {
				v := &d.Images.Details[k]
				if v.Path == job.diagnosticPath {
					v.OutboundReference, v.Action = "file_id", "uploaded"
					if job.reused {
						v.Action = "upload_reused"
					}
				}
			}
		}
		if d != nil && !slices.Contains(d.AdaptedFields, job.adapted) {
			d.AdaptedFields = append(d.AdaptedFields, job.adapted)
		}
	}
	// Describe the final representation, not the pre-upload or pre-trim input.
	if d != nil && d.Images != nil {
		d.Images.InlineImages = 0
		for _, item := range gjson.GetBytes(body, "input").Array() {
			for _, field := range []string{"content", "output"} {
				for _, part := range item.Get(field).Array() {
					url := part.Get("image_url").String()
					if part.Get("type").String() == "input_image" && len(url) >= 5 && strings.EqualFold(url[:5], "data:") {
						d.Images.InlineImages++
					}
				}
			}
		}
	}
	return body, used, nil
}

type bpsImageReference struct {
	Part  int
	Field string
	ID    string
}

// Rewrite each affected item once, and the full input once. Repeated sjson
// updates of the entire body copied tens of MB for every historical image.
func rewriteBPSImageReferences(body []byte, references map[int][]bpsImageReference) ([]byte, error) {
	if len(references) == 0 {
		return body, nil
	}
	parsed := gjson.GetBytes(body, "input").Array()
	items := make([]json.RawMessage, len(parsed))
	for i, item := range parsed {
		items[i] = json.RawMessage(item.Raw)
	}
	for index, replacements := range references {
		fields := map[string][]json.RawMessage{}
		for _, replacement := range replacements {
			parts, ok := fields[replacement.Field]
			if !ok {
				for _, part := range parsed[index].Get(replacement.Field).Array() {
					parts = append(parts, json.RawMessage(part.Raw))
				}
				fields[replacement.Field] = parts
			}
			encoded, err := sjson.DeleteBytes(parts[replacement.Part], "image_url")
			if err == nil {
				encoded, err = sjson.SetBytes(encoded, "file_id", replacement.ID)
			}
			if err != nil {
				return nil, err
			}
			parts[replacement.Part] = encoded
		}
		for field, parts := range fields {
			encoded, err := json.Marshal(parts)
			if err != nil {
				return nil, err
			}
			items[index], err = sjson.SetRawBytes(items[index], field, encoded)
			if err != nil {
				return nil, err
			}
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", encoded)
}

func bpsImageInputError(message string) *Error {
	return &Error{Code: "invalid_image_input", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

func uploadBPSImage(ctx context.Context, client *http.Client, headers http.Header, data []byte, mime string) (string, error) {
	name := map[string]string{"image/png": "image.png", "image/jpeg": "image.jpg", "image/gif": "image.gif", "image/webp": "image.webp"}[mime]
	return uploadBPSAttachment(ctx, client, headers, bpsFileAttachment{Data: data, Name: name, ContentType: mime})
}

func uploadBPSAttachment(ctx context.Context, client *http.Client, headers http.Header, file bpsFileAttachment) (result string, resultErr error) {
	started, status := time.Now(), 0
	defer func() {
		bpsTimingFromContext(ctx).uploaded(time.Since(started), len(file.Data), status, resultErr != nil)
	}()
	uploadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var buffer bytes.Buffer
	form := multipart.NewWriter(&buffer)
	name := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(file.Name)
	part, err := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="` + name + `"`}, "Content-Type": {file.ContentType}})
	if err != nil {
		return "", ErrInternalError("构建附件失败", err)
	}
	if _, err = part.Write(file.Data); err != nil {
		return "", ErrInternalError("构建附件失败", err)
	}
	if err = form.Close(); err != nil {
		return "", ErrInternalError("构建附件失败", err)
	}
	endpoint := CodexBPSBaseURL + "/attachments"
	if IsResinEnabled() {
		endpoint = BuildReverseProxyURL(endpoint)
	}
	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, endpoint, &buffer)
	if err != nil {
		return "", ErrInternalError("创建附件请求失败", nil)
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	// No redirects: account credentials are valid only for this fixed endpoint.
	localClient := *client
	localClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	traced, network := traceBPSHTTP(req)
	resp, err := localClient.Do(traced)
	phases := network.finish(err)
	if phases != nil {
		bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) {
			v.UploadHTTPObservations++
			if v.SlowestUploadHTTP == nil || phases.RoundTripMS > v.SlowestUploadHTTP.RoundTripMS {
				v.SlowestUploadHTTP = phases
			}
		})
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", bpsAttachmentFailure(ctx, headers, file.Name, "canceled", 0, nil, nil, ctx.Err())
		}
		return "", bpsAttachmentFailure(ctx, headers, file.Name, "transport", 0, nil, nil, err)
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", bpsAttachmentFailure(ctx, headers, file.Name, "http", status, resp.Header, raw, err)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", bpsAttachmentFailure(ctx, headers, file.Name, "canceled", status, resp.Header, nil, ctx.Err())
		}
		return "", bpsAttachmentFailure(ctx, headers, file.Name, "response_read", status, resp.Header, nil, err)
	}
	id := gjson.GetBytes(raw, "openai_file_id").String()
	if !validBPSAttachmentID(id) {
		return "", bpsAttachmentFailure(ctx, headers, file.Name, "response_shape", status, resp.Header, nil, nil)
	}
	return id, nil
}

// A cached handle can be removed upstream before the local TTL. Retry only a
// recognized missing/expired file reference, never an arbitrary model error.
func invalidateMissingBPSAttachments(resp *http.Response, used map[string]string) bool {
	if resp == nil || (resp.StatusCode != 400 && resp.StatusCode != 404) || len(used) == 0 {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body = &bpsPrefixReadCloser{Reader: io.MultiReader(bytes.NewReader(raw), resp.Body), closer: resp.Body}
	if err != nil || !gjson.ValidBytes(raw) {
		return false
	}
	var message, code string
	for _, path := range []string{"error", "detail.error.error", "detail.error"} {
		e := gjson.GetBytes(raw, path)
		if e.Get("message").String() != "" {
			message, code = strings.ToLower(e.Get("message").String()), e.Get("code").String()
			break
		}
	}
	if code != "file_not_found" && code != "file_expired" && !(strings.Contains(message, "file") && (strings.Contains(message, "not found") || strings.Contains(message, "expired") || strings.Contains(message, "does not exist"))) {
		return false
	}
	invalidated := false
	identified := make(map[string]bool)
	for _, id := range used {
		if bpsErrorNamesAttachment(message, strings.ToLower(id)) {
			identified[id] = true
		}
	}
	for key, id := range used {
		if identified[id] || len(identified) == 0 && (code == "file_not_found" || code == "file_expired") {
			bpsImages.forget(key, id)
			if resp.Request != nil {
				forgetSharedBPSAttachment(resp.Request.Context(), key, id)
			}
			invalidated = true
		}
	}
	return invalidated
}

func bpsErrorNamesAttachment(message, id string) bool {
	if id == "" {
		return false
	}
	for offset := 0; offset < len(message); {
		index := strings.Index(message[offset:], id)
		if index < 0 {
			return false
		}
		end := offset + index + len(id)
		if end == len(message) {
			return true
		}
		c := message[end]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return true
		}
		offset = end
	}
	return false
}

type bpsPrefixReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *bpsPrefixReadCloser) Close() error { return r.closer.Close() }
