package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
)

// Capture store writer. Captures are diagnostics: they are sampled per
// request, masked, truncated to database.PluginCaptureBodyLimit and written
// asynchronously in batches. A full queue drops captures instead of slowing
// the request path.

const (
	captureQueueSize    = 1024
	captureBatchSize    = 100
	captureFlushEvery   = time.Second
	captureWriteTimeout = 10 * time.Second
	// PluginCaptureRetention is how long capture rows are kept.
	PluginCaptureRetention = time.Duration(database.DefaultPluginCaptureRetentionDays) * 24 * time.Hour
)

// CaptureSink persists capture batches (implemented by *database.DB).
type CaptureSink interface {
	InsertPluginCaptures(ctx context.Context, captures []database.PluginCapture) error
}

// redactedCaptureHeaders are dropped to a fixed marker regardless of value.
var redactedCaptureHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
	"api-key":             {},
	"chatgpt-account-id":  {},
	"openai-organization": {},
	"x-codex-turn-state":  {},
}

type captureWriter struct {
	queue   chan database.PluginCapture
	sink    atomic.Pointer[CaptureSink]
	dropped atomic.Int64
	written atomic.Int64
	start   sync.Once
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{queue: make(chan database.PluginCapture, captureQueueSize)}
}

func (w *captureWriter) setSink(sink CaptureSink) {
	if sink == nil {
		w.sink.Store(nil)
		return
	}
	w.sink.Store(&sink)
}

func (w *captureWriter) enqueue(c database.PluginCapture) {
	select {
	case w.queue <- c:
	default:
		if w.dropped.Add(1)%100 == 1 {
			log.Printf("[transport-plugin] capture queue full, dropped %d captures so far", w.dropped.Load())
		}
	}
}

// StartCaptureWriter drains the capture queue until ctx ends. Safe to call
// more than once; only the first call starts the worker.
func (r *Registry) StartCaptureWriter(ctx context.Context) {
	r.capture.start.Do(func() { go r.capture.run(ctx) })
}

// CaptureStats reports how many captures were written and dropped.
func (r *Registry) CaptureStats() (written, dropped int64) {
	return r.capture.written.Load(), r.capture.dropped.Load()
}

func (w *captureWriter) run(ctx context.Context) {
	ticker := time.NewTicker(captureFlushEvery)
	defer ticker.Stop()
	batch := make([]database.PluginCapture, 0, captureBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if sink := w.sink.Load(); sink != nil {
			writeCtx, cancel := context.WithTimeout(context.Background(), captureWriteTimeout)
			if err := (*sink).InsertPluginCaptures(writeCtx, batch); err != nil {
				w.dropped.Add(int64(len(batch)))
				log.Printf("[transport-plugin] write %d captures failed: %v", len(batch), err)
			} else {
				w.written.Add(int64(len(batch)))
			}
			cancel()
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case c := <-w.queue:
					batch = append(batch, c)
					if len(batch) >= captureBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case c := <-w.queue:
			batch = append(batch, c)
			if len(batch) >= captureBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// captureSampled decides per request, so every attempt and direction of a
// sampled request is kept together. Requests without an ID sample randomly.
func captureSampled(requestID string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	if requestID == "" {
		return rand.Float64() < rate
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestID))
	return float64(h.Sum32()%10000) < rate*10000
}

// truncateCapture cuts s to at most limit bytes on a UTF-8 boundary.
func truncateCapture(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// maskCaptureBody truncates first (masking a multi-megabyte body would be
// wasteful), masks, then re-clamps because masking can lengthen short values.
func maskCaptureBody(body []byte) (string, bool) {
	text, truncated := truncateCapture(string(body), database.PluginCaptureBodyLimit)
	text = security.MaskSensitiveData(text)
	text, clamped := truncateCapture(text, database.PluginCaptureBodyLimit)
	return text, truncated || clamped
}

// maskCaptureHeaders records header with the credential headers redacted.
// Request headers (from clients) also have sensitive-looking values masked;
// upstream response headers are otherwise kept whole for analysis.
func maskCaptureHeaders(header http.Header) string { return captureHeaders(header, true) }

func captureHeaders(header http.Header, maskValues bool) string {
	if len(header) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string][]string, len(header))
	for _, key := range keys {
		if _, redact := redactedCaptureHeaders[strings.ToLower(key)]; redact {
			out[key] = []string{"[REDACTED]"}
			continue
		}
		values := make([]string, len(header[key]))
		for i, value := range header[key] {
			if maskValues {
				value = security.MaskSensitiveData(value)
			}
			values[i] = value
		}
		out[key] = values
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	text, _ := truncateCapture(string(encoded), database.PluginCaptureBodyLimit)
	return text
}

// captureRecorder records one attempt; a nil recorder is a no-op.
type captureRecorder struct {
	w    *captureWriter
	base database.PluginCapture
}

func (w *captureWriter) begin(state database.TransportPluginState, env *ReqEnv) *captureRecorder {
	if !state.CaptureEnabled || w.sink.Load() == nil {
		return nil
	}
	requestID := ""
	if env.Request != nil {
		requestID = env.Request.ID
	}
	if !captureSampled(requestID, state.CaptureSampleRate) {
		return nil
	}
	var accountID int64
	if env.Account != nil {
		accountID = env.Account.ID()
	}
	return &captureRecorder{w: w, base: database.PluginCapture{Plugin: state.ID, RequestID: requestID, AccountID: accountID, Attempt: env.Attempt}}
}

func (rec *captureRecorder) emit(c database.PluginCapture) {
	c.CreatedAt = time.Now()
	rec.w.enqueue(c)
}

func (rec *captureRecorder) request(env *ReqEnv) {
	if rec == nil {
		return
	}
	c := rec.base
	c.Direction = database.PluginCaptureDirectionRequest
	c.Headers = maskCaptureHeaders(env.Header)
	c.Body, c.Truncated = maskCaptureBody(env.Body)
	rec.emit(c)
}

func (rec *captureRecorder) failure(err error) {
	if rec == nil {
		return
	}
	c := rec.base
	c.Direction = database.PluginCaptureDirectionError
	c.ErrorKind = "execute_error"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		c.ErrorKind = "context_" + strings.ReplaceAll(err.Error(), " ", "_")
	}
	c.Body, c.Truncated = maskCaptureBody([]byte(err.Error()))
	rec.emit(c)
}

// response tees the raw upstream body (before any transform). The capture is
// emitted once, when the body reaches EOF, fails or is closed.
func (rec *captureRecorder) response(resp *http.Response) {
	if rec == nil || resp == nil {
		return
	}
	c := rec.base
	c.Direction = database.PluginCaptureDirectionResponse
	c.Status = resp.StatusCode
	c.Headers = captureHeaders(resp.Header, false)
	if resp.StatusCode >= 400 {
		c.ErrorKind = "http_" + http.StatusText(resp.StatusCode)
		c.ErrorKind = strings.ToLower(strings.ReplaceAll(c.ErrorKind, " ", "_"))
	}
	if resp.Body == nil {
		rec.emit(c)
		return
	}
	resp.Body = &captureTee{src: resp.Body, rec: rec, capture: c}
}

type captureTee struct {
	src     io.ReadCloser
	rec     *captureRecorder
	capture database.PluginCapture
	buf     []byte
	over    bool
	once    sync.Once
}

func (t *captureTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		room := database.PluginCaptureBodyLimit - len(t.buf)
		if room > 0 {
			t.buf = append(t.buf, p[:min(n, room)]...)
		}
		if n > room {
			t.over = true
		}
	}
	if err != nil {
		if !errors.Is(err, io.EOF) && t.capture.ErrorKind == "" {
			t.capture.ErrorKind = "body_read_error"
		}
		t.finish()
	}
	return n, err
}

func (t *captureTee) Close() error {
	t.finish()
	return t.src.Close()
}

func (t *captureTee) finish() {
	t.once.Do(func() {
		c := t.capture
		var truncated bool
		c.Body, truncated = maskCaptureBody(t.buf)
		c.Truncated = truncated || t.over
		t.rec.emit(c)
	})
}
