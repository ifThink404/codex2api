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

// Timing is attempt-local until normal response commit. The request duration
// includes admission and earlier attempts; a fast replacement cannot hide retries.
type upstreamFirstResponseTiming struct {
	enabled                    bool
	recorded                   bool
	requestStart, attemptStart time.Time
}

func (t *upstreamFirstResponseTiming) observe(headers http.Header, event gjson.Result, now time.Time) {
	if !t.enabled || t.recorded || !isLooseFirstTokenResult(event) {
		return
	}
	switch event.Get("type").String() {
	case "ping", "keepalive", "heartbeat":
		return
	}
	t.recorded = true
	requestMS := max(now.Sub(t.requestStart).Milliseconds(), 0)
	attemptMS := max(now.Sub(t.attemptStart).Milliseconds(), 0)
	headers.Set(upstreamTimingHeader, "v1-loose")
	headers.Set(upstreamFirstResponseHeader, strconv.FormatInt(requestMS, 10))
	headers.Set(upstreamAttemptFirstResponseHeader, strconv.FormatInt(min(attemptMS, requestMS), 10))
}

func clearUpstreamFirstResponseHeaders(headers http.Header) {
	for _, name := range []string{upstreamTimingHeader, upstreamFirstResponseHeader, upstreamAttemptFirstResponseHeader} {
		headers.Del(name)
	}
}

// Copy only at the existing commit boundary. Never flush or write a body here.
func relayUpstreamFirstResponseHeaders(c *gin.Context, headers http.Header) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	clearUpstreamFirstResponseHeaders(c.Writer.Header())
	if headers.Get(upstreamTimingHeader) != "v1-loose" {
		return
	}
	for _, name := range []string{upstreamTimingHeader, upstreamFirstResponseHeader, upstreamAttemptFirstResponseHeader} {
		c.Header(name, headers.Get(name))
	}
}
