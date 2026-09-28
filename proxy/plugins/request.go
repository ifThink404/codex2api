package plugins

import (
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

	mu        sync.Mutex
	bound     map[string]bool
	state     map[string]any
	usageMeta map[string]string
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
