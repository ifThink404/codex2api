package proxy

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// BPS organization rate limits. BPS answers two kinds of 429:
//
//   - organization-level TPM ("Rate limit reached for gpt-6-sol in
//     organization org-… on tokens per min (TPM): … Please try again in
//     29ms."): a quota shared by every account on BPS for that model, not an
//     account problem. The attempt waits the hinted time (capped, with jitter)
//     and retries on the SAME account, at most bpsOrgRetryLimit times per
//     request, without any account cooldown or strike;
//   - per-account ("429: Rate limit exceeded"): the account BPS backoff.
//
// An org limit without a usable hint (missing or above bpsOrgRetryMaxWait)
// takes the per-account path. The org limit can arrive as an HTTP 429 or as a
// failed event at the start of a 200 stream (no headers then), so the
// stream's first frames are inspected before the response is handed on.

const (
	bpsOrgRetryLimit   = 3
	bpsOrgRetryMaxWait = 5 * time.Second
	// bpsOrgPeekTimeout bounds how long a stream start is held for inspection.
	bpsOrgPeekTimeout = 5 * time.Second
	bpsOrgBurstWindow = time.Second
)

var bpsOrgRetryAfter = regexp.MustCompile(`(?i)please try again in ([0-9]+(?:\.[0-9]+)?)\s*(ms|s)\b`)

// bpsOrgRateLimitHint reports whether message is an organization-level rate
// limit and returns its "Please try again in" hint (0 when absent).
func bpsOrgRateLimitHint(message string) (time.Duration, bool) {
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "rate limit reached for") || !strings.Contains(lower, "in organization") {
		return 0, false
	}
	match := bpsOrgRetryAfter.FindStringSubmatch(message)
	if match == nil {
		return 0, true
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil || value < 0 {
		return 0, true
	}
	unit := time.Second
	if strings.EqualFold(match[2], "ms") {
		unit = time.Millisecond
	}
	return time.Duration(value * float64(unit)), true
}

// bpsOrgRetryable reports an org limit the attempt may wait out itself.
func bpsOrgRetryable(message string) (time.Duration, bool) {
	hint, org := bpsOrgRateLimitHint(message)
	return hint, org && hint > 0 && hint <= bpsOrgRetryMaxWait
}

// bpsErrorMessageOf is the provider message of an error body or event source.
func bpsErrorMessageOf(source gjson.Result) string {
	if source.Type == gjson.String {
		return source.String()
	}
	return source.Get("message").String()
}

// bpsOrgModelPause is a short process-wide per-model pause set when several
// org limits for one model arrive within a second, so concurrent attempts do
// not all retry into the same exhausted window.
type bpsOrgModelPause struct {
	mu    sync.Mutex
	last  map[string]time.Time
	until map[string]time.Time
}

var bpsOrgPauses = &bpsOrgModelPause{}

// observe notes an org limit for model and returns how long to wait before
// retrying: the hint, or the model pause if longer, capped.
func (p *bpsOrgModelPause) observe(model string, hint time.Duration, now time.Time) time.Duration {
	model = strings.ToLower(strings.TrimSpace(model))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		p.last, p.until = map[string]time.Time{}, map[string]time.Time{}
	}
	if prev, ok := p.last[model]; ok && now.Sub(prev) < bpsOrgBurstWindow {
		if until := now.Add(hint); until.After(p.until[model]) {
			p.until[model] = until
		}
	}
	p.last[model] = now
	wait := hint
	if paused := p.until[model].Sub(now); paused > wait {
		wait = paused
	}
	return min(wait, bpsOrgRetryMaxWait)
}

// bpsOrgJitter adds up to 20% (at least 5ms) so retries spread out.
func bpsOrgJitter(wait time.Duration) time.Duration {
	spread := max(wait/5, 5*time.Millisecond)
	return min(wait+time.Duration(rand.Int64N(int64(spread))), bpsOrgRetryMaxWait)
}

// bpsSleep waits d unless ctx ends first.
func bpsSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// bpsPeekOrgRateLimit inspects a BPS response for an org rate limit the
// attempt may retry: an HTTP 429 body, or a failed event before any output in
// a 200 stream. The response body stays readable either way.
func bpsPeekOrgRateLimit(resp *http.Response) (time.Duration, bool) {
	if resp == nil || resp.Body == nil {
		return 0, false
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body = &bpsPrefixReadCloser{Reader: io.MultiReader(bytes.NewReader(raw), resp.Body), closer: resp.Body}
		return bpsOrgRetryable(bpsErrorMessageOf(bpsErrorBodySource(gjson.ParseBytes(raw))))
	}
	if resp.StatusCode != http.StatusOK || !bpsStreamIsEventStream(resp.Header.Get("Content-Type")) {
		return 0, false
	}
	peek := newBPSStreamPeek(resp.Body)
	resp.Body = peek
	return peek.decide(bpsOrgPeekTimeout)
}

// bpsStreamPeek holds the start of a stream until its first frame other than
// response.created / in_progress shows whether it is an org rate limit. All
// data flows through one goroutine into a pipe; after the decision (or the
// peek timeout) frames pass through unchanged.
type bpsStreamPeek struct {
	*io.PipeReader
	upstream  io.ReadCloser
	mu        sync.Mutex
	committed bool
	decision  chan time.Duration
	once      sync.Once
}

func newBPSStreamPeek(upstream io.ReadCloser) *bpsStreamPeek {
	reader, writer := io.Pipe()
	p := &bpsStreamPeek{PipeReader: reader, upstream: upstream, decision: make(chan time.Duration, 1)}
	go p.run(writer)
	return p
}

// decided hands the decision to decide unless it already gave up waiting;
// it reports whether the stream start is the org limit being retried.
func (p *bpsStreamPeek) decided(hint time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.committed {
		return false
	}
	p.committed = true
	p.decision <- hint
	return hint > 0
}

func (p *bpsStreamPeek) run(writer *io.PipeWriter) {
	var buffered [][]byte
	peeking := true
	flush := func() error {
		for _, raw := range buffered {
			if _, err := writer.Write(raw); err != nil {
				return err
			}
		}
		buffered = nil
		return nil
	}
	var writeErr error
	err := readBPSStreamFrames(p.upstream, func(frame bpsStreamFrame) bool {
		if !peeking {
			_, writeErr = writer.Write(frame.raw)
			return writeErr == nil
		}
		buffered = append(buffered, frame.raw)
		event := gjson.ParseBytes(frame.data)
		switch event.Get("type").String() {
		case "response.created", "response.in_progress":
			return true
		}
		if source, failed := bpsTerminalEventSource(event); failed {
			if hint, ok := bpsOrgRetryable(bpsErrorMessageOf(source)); ok && p.decided(hint) {
				// The attempt retries; nobody reads this stream.
				return false
			}
		}
		p.decided(0)
		peeking = false
		writeErr = flush()
		return writeErr == nil
	})
	if peeking {
		p.decided(0)
		if writeErr == nil {
			writeErr = flush()
		}
	}
	if writeErr != nil {
		err = writeErr
	}
	_ = writer.CloseWithError(err)
}

// decide waits for the peek decision, at most timeout; a stream still
// silent then is passed on and never retried.
func (p *bpsStreamPeek) decide(timeout time.Duration) (time.Duration, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case hint := <-p.decision:
		return hint, hint > 0
	case <-timer.C:
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.committed {
			p.committed = true
			return 0, false
		}
		hint := <-p.decision
		return hint, hint > 0
	}
}

func (p *bpsStreamPeek) Close() error {
	var err error
	p.once.Do(func() { err = p.upstream.Close() })
	_ = p.PipeReader.Close()
	return err
}
