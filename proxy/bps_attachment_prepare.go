package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
)

type bpsAttachmentPreparationKey struct{}
type bpsAttachmentSource struct{ kind, value, filename string }
type bpsPreparedAttachment struct {
	file     bpsFileAttachment
	identity string
}
type bpsAttachmentPreparation struct {
	mu     sync.Mutex
	items  map[bpsAttachmentSource]bpsPreparedAttachment
	bytes  int64
	closed bool
}

// Decoded bytes are request-local, never persisted or mixed between users.
// Retention is opportunistic: at most 8 MiB/request and 64 MiB/process. Large
// attachments still work but are decoded again if a retry really needs them.
var bpsPreparedBytes atomic.Int64

func withBPSAttachmentPreparation(ctx context.Context) (context.Context, func()) {
	if _, ok := ctx.Value(bpsAttachmentPreparationKey{}).(*bpsAttachmentPreparation); ok {
		return ctx, func() {}
	}
	m := &bpsAttachmentPreparation{items: make(map[bpsAttachmentSource]bpsPreparedAttachment)}
	cleanup := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.closed {
			m.closed = true
			bpsPreparedBytes.Add(-m.bytes)
			m.items = nil
		}
	}
	stop := context.AfterFunc(ctx, cleanup)
	return context.WithValue(ctx, bpsAttachmentPreparationKey{}, m), func() { stop(); cleanup() }
}

func prepareBPSAttachment(ctx context.Context, kind, value, filename string) (bpsPreparedAttachment, error) {
	m, _ := ctx.Value(bpsAttachmentPreparationKey{}).(*bpsAttachmentPreparation)
	key := bpsAttachmentSource{kind, value, filename}
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if prepared, ok := m.items[key]; ok {
			bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.PreparationReuses++ })
			return prepared, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return bpsPreparedAttachment{}, err
	}
	var file bpsFileAttachment
	var err error
	if kind == "image" {
		if len(value) > security.MaxRequestBodySize {
			return bpsPreparedAttachment{}, bpsImageInputError("图片数据超过请求大小限制。")
		}
		metadata, encoded, found := strings.Cut(value, ",")
		if !found || !strings.HasSuffix(strings.ToLower(metadata), ";base64") {
			return bpsPreparedAttachment{}, bpsImageInputError("图片数据无效或格式不受支持，请使用 PNG、JPEG、GIF 或 WebP 图片。")
		}
		file.Data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return bpsPreparedAttachment{}, bpsImageInputError("图片 Base64 数据不完整或无效，请重新附加图片。")
		}
		file.ContentType = http.DetectContentType(file.Data)
		switch file.ContentType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return bpsPreparedAttachment{}, bpsImageInputError("图片数据无效或格式不受支持，请使用 PNG、JPEG、GIF 或 WebP 图片。")
		}
	} else {
		file, err = decodeBPSFileData(value, filename)
		if err != nil {
			return bpsPreparedAttachment{}, err
		}
	}
	digest := sha256.Sum256(file.Data)
	identity := hex.EncodeToString(digest[:])
	if kind == "file" {
		raw, _ := json.Marshal([]string{file.Name, file.ContentType, identity})
		identity = string(raw)
	}
	prepared := bpsPreparedAttachment{file: file, identity: identity}
	bpsTimingFromContext(ctx).update(func(v *bpsTimingValues) { v.AttachmentDecodes++; v.AttachmentHashes++ })
	// Count retained keys too: the base64 input can outweigh the decoded bytes.
	size := int64(len(value) + len(filename) + len(file.Data) + len(identity) + len(file.Name) + 256)
	if m != nil && !m.closed && len(m.items) < 128 && m.bytes+size <= 8<<20 {
		for total := bpsPreparedBytes.Load(); total+size <= 64<<20; total = bpsPreparedBytes.Load() {
			if bpsPreparedBytes.CompareAndSwap(total, total+size) {
				// GJSON strings can refer to a much larger request buffer.
				key.value, key.filename = strings.Clone(value), strings.Clone(filename)
				m.items[key] = prepared
				m.bytes += size
				break
			}
		}
	}
	return prepared, nil
}

func bpsPreparedUploadKey(account *auth.Account, kind string, prepared bpsPreparedAttachment) string {
	return codexIdentityDigest("bps-"+kind+"-upload-v1", fmt.Sprintf("%d:%s", account.ID(), account.EffectiveAccountID()), prepared.identity)
}
