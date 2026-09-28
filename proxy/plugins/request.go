package plugins

import (
	"context"
	"net/http"
	"sync"
)

// Request is the per-inbound-request context shared by every attempt. Core
// creates it on the first Resolve; plugins keep their own state in it.
type Request struct {
	// ID is the gateway request_id (joins usage_logs and plugin_captures).
	ID   string
	Kind RequestKind
	// Body is the raw inbound body; Header the inbound headers. Read-only.
	Body     []byte
	Header   http.Header
	APIKeyID int64
	// Model is the effective model core schedules for.
	Model string
	// Host is an opaque handle core attaches for plugins compiled into the
	// same package (the proxy handler and gin context). Plugins in other
	// packages must not rely on it.
	Host any

	mu        sync.Mutex
	bound     map[string]bool
	state     map[string]any
	usageMeta map[string]string
	usageEnd  map[string]string
	usageKind map[string]string
	served    string
	attempts  int
}

// NewRequest builds a Request. Core calls it once per inbound request.
func NewRequest(id string, kind RequestKind, body []byte, header http.Header, apiKeyID int64) *Request {
	return &Request{ID: id, Kind: kind, Body: body, Header: header, APIKeyID: apiKeyID}
}

// State returns the value a plugin stored with SetState.
func (r *Request) State(pluginID string) any {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state[pluginID]
}

// SetState stores plugin-owned per-request state.
func (r *Request) SetState(pluginID string, value any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = map[string]any{}
	}
	r.state[pluginID] = value
}

// SetUsageMeta records the JSON written to usage_logs.plugin_meta for rows
// logged while pluginID serves this request.
func (r *Request) SetUsageMeta(pluginID, metaJSON string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usageMeta == nil {
		r.usageMeta = map[string]string{}
	}
	r.usageMeta[pluginID] = metaJSON
}

// SetUsageUpstreamEndpoint overrides Meta.UpstreamEndpoint in usage rows
// logged while pluginID serves this request (e.g. a /compact variant).
func (r *Request) SetUsageUpstreamEndpoint(pluginID, endpoint string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usageEnd == nil {
		r.usageEnd = map[string]string{}
	}
	r.usageEnd[pluginID] = endpoint
}

// SetUsageErrorKind records usage_logs.upstream_error_kind for rows logged
// while pluginID serves this request and core recorded no error kind (e.g. a
// successful response the plugin had to complete itself).
func (r *Request) SetUsageErrorKind(pluginID, kind string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usageKind == nil {
		r.usageKind = map[string]string{}
	}
	r.usageKind[pluginID] = kind
}

// UsageErrorKind returns the kind set with SetUsageErrorKind.
func (r *Request) UsageErrorKind(pluginID string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usageKind[pluginID]
}

// Transport returns the transport of the latest resolved attempt ("" before
// the first attempt) and that transport's usage metadata.
func (r *Request) Transport() (transport, meta string) {
	if r == nil {
		return "", ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.served, r.usageMeta[r.served]
}

// UsageUpstreamEndpoint returns the endpoint set with SetUsageUpstreamEndpoint.
func (r *Request) UsageUpstreamEndpoint(pluginID string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usageEnd[pluginID]
}

// Served returns the transport of the latest resolved attempt.
func (r *Request) Served() string {
	transport, _ := r.Transport()
	return transport
}

type requestContextKey struct{}

// WithRequest attaches req to ctx so hooks without a Request parameter
// (Admissible) can reach it.
func WithRequest(ctx context.Context, req *Request) context.Context {
	return context.WithValue(ctx, requestContextKey{}, req)
}

// RequestFromContext returns the Request attached with WithRequest.
func RequestFromContext(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}
	req, _ := ctx.Value(requestContextKey{}).(*Request)
	return req
}

// beginAttempt returns the 1-based attempt index and the prior transport.
func (r *Request) beginAttempt() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	return r.attempts, r.served
}

func (r *Request) setServed(transport string) {
	r.mu.Lock()
	r.served = transport
	r.mu.Unlock()
}

// markBound reports whether pluginID was not yet bound (and marks it).
func (r *Request) markBound(pluginID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bound[pluginID] {
		return false
	}
	if r.bound == nil {
		r.bound = map[string]bool{}
	}
	r.bound[pluginID] = true
	return true
}
