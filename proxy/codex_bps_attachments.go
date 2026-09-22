package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	c.mu.Lock()
	now := time.Now()
	for k, e := range c.entries {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	if e := c.entries[key]; e != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-e.ready:
			return e.id, e.err == nil, e.err
		}
	}
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
		} else {
			// Do not let many concurrent unique uploads grow the cache unbounded.
			c.mu.Unlock()
			id, err := upload()
			return id, false, err
		}
	}
	e := &bpsAttachmentEntry{ready: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()
	id, err := upload()
	c.mu.Lock()
	e.id, e.err, e.expires = id, err, time.Now().Add(bpsAttachmentCacheTTL)
	if err != nil {
		delete(c.entries, key)
	}
	close(e.ready)
	c.mu.Unlock()
	return id, false, err
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

// BPS user attachments use input_image.file_id, unlike inline tool images.
// Keep the same message, content order, detail and all other fields. Never
// manufacture assistant tool calls or fetch arbitrary remote image URLs.
func prepareBPSUserImageAttachments(ctx context.Context, account *auth.Account, body []byte, d *CodexBPSDiagnostic, upload func(context.Context, []byte, string) (string, error)) ([]byte, map[string]string, error) {
	used := make(map[string]string)
	for i, item := range gjson.GetBytes(body, "input").Array() {
		if typ := item.Get("type").String(); (typ != "message" && typ != "") || item.Get("role").String() != "user" {
			continue
		}
		for j, part := range item.Get("content").Array() {
			if part.Get("type").String() != "input_image" || part.Get("file_id").String() != "" {
				continue
			}
			url := part.Get("image_url").String()
			if len(url) < 5 || !strings.EqualFold(url[:5], "data:") {
				continue
			}
			path := fmt.Sprintf("input.%d.content.%d", i, j)
			detail := codexBPSImageDetail{}
			normalized := normalizeBPSImageDataURL(url, &detail)
			if detail.DetectedMIME == "" {
				return nil, nil, bpsImageInputError("用户消息中的图片数据无效或格式不受支持，请使用 PNG、JPEG、GIF 或 WebP 图片。")
			}
			_, encoded, _ := strings.Cut(normalized, ",")
			// Bound allocations by the same limit as the gateway's request body.
			if len(encoded) > security.MaxRequestBodySize {
				return nil, nil, bpsImageInputError("图片数据超过请求大小限制。")
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, nil, bpsImageInputError("图片 Base64 数据不完整或无效，请重新附加图片。")
			}
			key := bpsImageUploadKey(account, data)
			id, reused, err := bpsImages.resolve(ctx, key, func() (string, error) { return upload(ctx, data, detail.DetectedMIME) })
			if err != nil {
				return nil, nil, err
			}
			used[key] = id
			body, err = sjson.DeleteBytes(body, path+".image_url")
			if err == nil {
				body, err = sjson.SetBytes(body, path+".file_id", id)
			}
			if err != nil {
				return nil, nil, ErrInternalError("构建图片附件引用失败", err)
			}
			if d != nil && d.Images != nil {
				if reused {
					d.Images.UploadReused++
				} else {
					d.Images.Uploaded++
				}
				for k := range d.Images.Details {
					v := &d.Images.Details[k]
					if v.Path == fmt.Sprintf("input[%d].content[%d]", i, j) {
						v.OutboundReference = "file_id"
						v.Action = "uploaded"
						if reused {
							v.Action = "upload_reused"
						}
					}
				}
			}
		}
	}
	if len(used) > 0 && d != nil && !slices.Contains(d.AdaptedFields, "user input image → uploaded attachment") {
		d.AdaptedFields = append(d.AdaptedFields, "user input image → uploaded attachment")
	}
	return body, used, nil
}

func bpsImageInputError(message string) *Error {
	return &Error{Code: "invalid_image_input", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

func uploadBPSImage(ctx context.Context, client *http.Client, headers http.Header, data []byte, mime string) (string, error) {
	name := map[string]string{"image/png": "image.png", "image/jpeg": "image.jpg", "image/gif": "image.gif", "image/webp": "image.webp"}[mime]
	return uploadBPSAttachment(ctx, client, headers, bpsFileAttachment{Data: data, Name: name, ContentType: mime})
}

func uploadBPSAttachment(ctx context.Context, client *http.Client, headers http.Header, file bpsFileAttachment) (string, error) {
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
	resp, err := localClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrUpstream(http.StatusBadGateway, "附件上传失败，请稍后重试。", nil)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := resp.StatusCode
		if status < 400 {
			status = http.StatusBadGateway
		}
		// Raw errors can echo paths, credentials or attachment identifiers.
		return "", ErrUpstream(status, "附件上传失败，请确认文件可由当前上游处理后重试。", nil)
	}
	if err != nil {
		return "", ErrUpstream(http.StatusBadGateway, "读取附件上传结果失败。", nil)
	}
	id := gjson.GetBytes(raw, "openai_file_id").String()
	if !strings.HasPrefix(id, "file-") || len(id) > 256 || strings.ContainsAny(id, " \t\r\n/\\") {
		return "", ErrUpstream(http.StatusBadGateway, "附件上传结果缺少有效的文件引用。", nil)
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
	for key, id := range used {
		if strings.Contains(strings.ToLower(message), strings.ToLower(id)) || code == "file_not_found" || code == "file_expired" {
			bpsImages.forget(key, id)
			invalidated = true
		}
	}
	return invalidated
}

type bpsPrefixReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *bpsPrefixReadCloser) Close() error { return r.closer.Close() }
