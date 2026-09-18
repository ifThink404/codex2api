package proxy

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	upstreamTimingHeader               = "X-Codex2API-Response-Timing"
	upstreamFirstResponseHeader        = "X-Codex2API-First-Response-Ms"
	upstreamAttemptFirstResponseHeader = "X-Codex2API-Attempt-First-Response-Ms"
)

// upstreamFirstResponseTiming is the only source of a published timing report.
// It stays attempt-local until the normal response commit: the request duration
// includes admission and earlier attempts, so a fast replacement cannot hide
// retries, while the attempt duration covers only the winning attempt.
//
// What this gateway measured is never read back out of an upstream response
// header. A relay account's upstream is an arbitrary base URL — possibly another
// codex2api with this switch on — and copying its X-Codex2API-* headers would
// attribute that gateway's first-response time to this one. Keeping the record
// in the attempt's own struct also survives the continue-thinking fold, which
// replaces the *http.Response before the buffered commit reads it.
type upstreamFirstResponseTiming struct {
	enabled                    bool
	recorded                   bool
	requestStart, attemptStart time.Time
	requestMS, attemptMS       int64
}

func (t *upstreamFirstResponseTiming) observe(event gjson.Result, now time.Time) {
	if !t.enabled || t.recorded || !isLooseFirstTokenResult(event) {
		return
	}
	switch event.Get("type").String() {
	case "ping", "keepalive", "heartbeat":
		return
	}
	t.recorded = true
	t.requestMS = max(now.Sub(t.requestStart).Milliseconds(), 0)
	t.attemptMS = min(max(now.Sub(t.attemptStart).Milliseconds(), 0), t.requestMS)
}

// clearUpstreamFirstResponseHeaders drops the three private names from a header
// map. On an upstream response it keeps an echoed value out of anything that may
// forward upstream headers; on the downstream writer it drops the staged values
// of an attempt that turned out not to win.
func clearUpstreamFirstResponseHeaders(headers http.Header) {
	for _, name := range []string{upstreamTimingHeader, upstreamFirstResponseHeader, upstreamAttemptFirstResponseHeader} {
		headers.Del(name)
	}
}

// relayUpstreamFirstResponseHeaders publishes this gateway's own measurement at
// an existing commit boundary. It never flushes or writes a body. A nil timing —
// which is what every non-official path has — clears stale values and publishes
// nothing.
func relayUpstreamFirstResponseHeaders(c *gin.Context, timing *upstreamFirstResponseTiming) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	clearUpstreamFirstResponseHeaders(c.Writer.Header())
	if timing == nil || !timing.recorded {
		return
	}
	c.Header(upstreamTimingHeader, "v1-loose")
	c.Header(upstreamFirstResponseHeader, strconv.FormatInt(timing.requestMS, 10))
	c.Header(upstreamAttemptFirstResponseHeader, strconv.FormatInt(timing.attemptMS, 10))
}
