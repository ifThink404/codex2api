package proxy

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Offsets share the HTTP request start. Phase durations can overlap (for
// example connection acquisition contains DNS/dial/TLS), so do not sum them.
// Missing callbacks remain absent, rather than claiming zero network latency.
type bpsHTTPPhases struct {
	Source              string `json:"source"`
	RoundTripMS         int64  `json:"roundtrip_ms"`
	ConnectionAcquireMS *int64 `json:"connection_acquire_ms,omitempty"`
	ConnectionReused    *bool  `json:"connection_reused,omitempty"`
	DNSMS               *int64 `json:"dns_ms,omitempty"`
	TCPMS               *int64 `json:"tcp_ms,omitempty"`
	DialMS              *int64 `json:"dial_ms,omitempty"` // uTLS dial includes proxy negotiation.
	TLSMS               *int64 `json:"tls_ms,omitempty"`
	RequestWriteMS      *int64 `json:"request_write_ms,omitempty"`
	WroteRequestMS      *int64 `json:"wrote_request_ms,omitempty"`
	FirstResponseByteMS *int64 `json:"first_response_byte_ms,omitempty"`
	ResponseWaitMS      *int64 `json:"response_wait_ms,omitempty"`
	Failed              bool   `json:"failed,omitempty"`
}

type bpsHTTPTraceKey struct{}
type bpsHTTPTrace struct {
	mu                   sync.Mutex
	started, conn, wrote time.Time
	dns, dial, tls       time.Time
	connects             map[string]time.Time
	values               bpsHTTPPhases
}

func bpsHTTPTraceFromContext(ctx context.Context) *bpsHTTPTrace {
	t, _ := ctx.Value(bpsHTTPTraceKey{}).(*bpsHTTPTrace)
	return t
}

func (t *bpsHTTPTrace) change(fn func()) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fn()
}

func bpsElapsed(start, end time.Time) *int64 {
	if start.IsZero() {
		return nil
	}
	v := max(end.Sub(start).Milliseconds(), 0)
	return &v
}

func (t *bpsHTTPTrace) gotConnection(reused bool) {
	t.change(func() {
		t.conn = time.Now()
		t.values.ConnectionAcquireMS = bpsElapsed(t.started, t.conn)
		t.values.ConnectionReused = &reused
	})
}

func (t *bpsHTTPTrace) phase(name string, start bool) {
	t.change(func() {
		var stamp *time.Time
		var field **int64
		switch name {
		case "dns":
			stamp, field = &t.dns, &t.values.DNSMS
		case "dial":
			stamp, field = &t.dial, &t.values.DialMS
		case "tls":
			stamp, field = &t.tls, &t.values.TLSMS
		}
		if stamp == nil {
			return
		}
		if start {
			*stamp = time.Now()
		} else {
			*field = bpsElapsed(*stamp, time.Now())
		}
	})
}

func traceBPSHTTP(req *http.Request) (*http.Request, *bpsHTTPTrace) {
	if bpsTimingFromContext(req.Context()) == nil {
		return req, nil
	}
	t := &bpsHTTPTrace{started: time.Now(), values: bpsHTTPPhases{Source: "httptrace"}, connects: make(map[string]time.Time)}
	trace := &httptrace.ClientTrace{
		GotConn:      func(info httptrace.GotConnInfo) { t.gotConnection(info.Reused) },
		DNSStart:     func(httptrace.DNSStartInfo) { t.phase("dns", true) },
		DNSDone:      func(httptrace.DNSDoneInfo) { t.phase("dns", false) },
		ConnectStart: func(network, addr string) { t.change(func() { t.connects[network+":"+addr] = time.Now() }) },
		ConnectDone: func(network, addr string, _ error) {
			t.change(func() {
				key := network + ":" + addr
				if elapsed := bpsElapsed(t.connects[key], time.Now()); elapsed != nil {
					if t.values.TCPMS != nil {
						*elapsed += *t.values.TCPMS
					}
					t.values.TCPMS = elapsed
				}
				delete(t.connects, key)
			})
		},
		TLSHandshakeStart: func() { t.phase("tls", true) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { t.phase("tls", false) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				return
			}
			t.change(func() {
				t.wrote = time.Now()
				t.values.WroteRequestMS = bpsElapsed(t.started, t.wrote)
				t.values.RequestWriteMS = bpsElapsed(t.conn, t.wrote)
			})
		},
		GotFirstResponseByte: func() {
			t.change(func() {
				if t.values.FirstResponseByteMS != nil {
					return
				}
				now := time.Now()
				t.values.FirstResponseByteMS = bpsElapsed(t.started, now)
				t.values.ResponseWaitMS = bpsElapsed(t.wrote, now)
			})
		},
	}
	ctx := context.WithValue(req.Context(), bpsHTTPTraceKey{}, t)
	return req.WithContext(httptrace.WithClientTrace(ctx, trace)), t
}

func (t *bpsHTTPTrace) finish(err error) *bpsHTTPPhases {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.values
	v.RoundTripMS, v.Failed = time.Since(t.started).Milliseconds(), err != nil
	return &v
}
