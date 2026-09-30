package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// Image-refusal ladder (adopted from upstream's official Excel BPS adapter
// and merged with fj's missing-file invalidation). When BPS refuses a body
// because of its images, the request is retried with cached image handles
// re-uploaded, and if every handle was already fresh, with the images
// replaced by a text note. Upload failures of history images degrade to the
// same note; only an image of the latest user turn fails the request, since
// the user expects that image to be seen.

const (
	bpsImageOmittedRefused = "[image content omitted: the upstream did not accept it]"
	bpsImageOmittedUpload  = "[image content omitted: it could not be uploaded]"
)

type bpsImageLadderKey struct{}

type bpsImageLadderStage int

const (
	bpsImagesDefault bpsImageLadderStage = iota
	bpsImagesReupload
	bpsImagesOmit
)

// bpsImageLadder is the per-attempt ladder state. prepareBPSUserImageAttachments
// records the image handles each body carries; the send loop reads them.
type bpsImageLadder struct {
	mu     sync.Mutex
	stage  bpsImageLadderStage
	used   map[string]string
	reused bool
	inline bool
}

func withBPSImageLadder(ctx context.Context) (context.Context, *bpsImageLadder) {
	ladder := &bpsImageLadder{}
	return context.WithValue(ctx, bpsImageLadderKey{}, ladder), ladder
}

func bpsImageLadderFrom(ctx context.Context) *bpsImageLadder {
	ladder, _ := ctx.Value(bpsImageLadderKey{}).(*bpsImageLadder)
	return ladder
}

func (l *bpsImageLadder) omitting() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stage == bpsImagesOmit
}

// record describes the images of the body about to be sent.
func (l *bpsImageLadder) record(used map[string]string, reused, inline bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used, l.reused, l.inline = used, reused, inline
}

// advance picks the next step after BPS refused a body with images: re-upload
// cached handles first, then drop the images. It reports false when the
// ladder is exhausted or the body carried no images.
func (l *bpsImageLadder) advance(ctx context.Context) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.used) == 0 && !l.inline {
		return false
	}
	switch {
	case l.stage == bpsImagesOmit:
		return false
	case l.stage == bpsImagesDefault && l.reused:
		// Fresh uploads always follow a re-upload, so this cannot repeat.
		for key, id := range l.used {
			bpsImages.forget(key, id)
			forgetSharedBPSAttachment(ctx, key, id)
		}
		l.stage = bpsImagesReupload
	default:
		l.stage = bpsImagesOmit
	}
	return true
}

// bpsImageRefusal reports whether a rejected response may have failed because
// of its images. BPS answers invalid image placement with a bare 422 schema
// error; a 400 counts only when it names images or files, so unrelated 400s
// (context length, bad fields) are not retried. The body stays readable.
func bpsImageRefusal(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return true
	}
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body = &bpsPrefixReadCloser{Reader: io.MultiReader(bytes.NewReader(raw), resp.Body), closer: resp.Body}
	message := strings.ToLower(gjson.GetBytes(raw, "error.message").String() + " " + gjson.GetBytes(raw, "detail").String() + " " + gjson.GetBytes(raw, "detail.error.message").String())
	return strings.Contains(message, "image") || strings.Contains(message, "file") || strings.Contains(message, "attachment")
}

// bpsImageOmittedPart is the text part that replaces an omitted image.
func bpsImageOmittedPart(note string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"type": "input_text", "text": note})
	return raw
}

// bpsLatestUserIndex returns the index of the last user message in input; an
// image at or after it belongs to the latest turn.
func bpsLatestUserIndex(input []gjson.Result) int {
	last := -1
	for i, item := range input {
		if kind := item.Get("type").String(); (kind == "message" || kind == "") && item.Get("role").String() == "user" {
			last = i
		}
	}
	return last
}

// bpsHistoryImageDegradable reports whether a history image that could not
// be prepared or uploaded may become a note. A cancelled request or an upload
// rate limit keeps failing the request, so the account cooldown and retry on
// another account still apply.
func bpsHistoryImageDegradable(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !rateLimitRequestError(err)
}
