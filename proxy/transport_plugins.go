package proxy

import (
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

// Transport plugin glue. Every handler site calls exactly one of these, so
// the plugin framework (proxy/plugins) stays out of the handler control flow.

const contextTransportPluginRequest = "transport_plugin_request"

// transportPluginRequest returns the per-inbound-request plugin context,
// creating it on first use. A downstream WebSocket reuses one gin.Context for
// many turns but resets the request trace per turn, so a changed request ID
// starts a fresh plugin request.
func transportPluginRequest(c *gin.Context, kind plugins.RequestKind, body []byte) *plugins.Request {
	requestID := snapshotUpstreamTrace(c.Request.Context()).RequestID
	if value, ok := c.Get(contextTransportPluginRequest); ok {
		if req, ok := value.(*plugins.Request); ok && req.ID == requestID {
			return req
		}
	}
	var apiKeyID int64
	if value, ok := c.Get(contextAPIKeyID); ok {
		switch typed := value.(type) {
		case int64:
			apiKeyID = typed
		case int:
			apiKeyID = int64(typed)
		}
	}
	req := plugins.NewRequest(requestID, kind, body, c.Request.Header, apiKeyID)
	c.Set(contextTransportPluginRequest, req)
	return req
}

// resolveTransportPlugin decides, once per attempt, whether a transport plugin
// serves it. nil means the native path (including the inline BPS, Excel BPS
// and relay paths, which keep their own executors). The decision is recorded
// on the request so usage logs attribute the attempt to its transport.
func (h *Handler) resolveTransportPlugin(c *gin.Context, account *auth.Account, model string, kind plugins.RequestKind, body []byte) *plugins.Route {
	registry := plugins.Default()
	if len(registry.Plugins()) == 0 || c == nil || c.Request == nil || account == nil {
		return nil
	}
	req := transportPluginRequest(c, kind, body)
	if account.IsRelayStyle() || account.CodexBPSEnabled() || account.IsExcelBPSAvailableForModel(model) {
		registry.MarkNative(req)
		return nil
	}
	return registry.Resolve(c.Request.Context(), req, account, model, kind)
}

// applyTransportPluginFilter adds the plugins' Admissible veto to the
// scheduler account filter.
func (h *Handler) applyTransportPluginFilter(c *gin.Context, model string, kind plugins.RequestKind, filter auth.AccountFilter) auth.AccountFilter {
	return plugins.Default().AccountFilter(c.Request.Context(), kind, model, filter)
}

// populateTransportPluginUsage stamps usage rows logged while a plugin serves
// the request with the plugin ID, its metadata and its upstream endpoint.
func populateTransportPluginUsage(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || input.Transport != "" {
		return
	}
	value, ok := c.Get(contextTransportPluginRequest)
	if !ok {
		return
	}
	req, _ := value.(*plugins.Request)
	if req == nil || req.ID != snapshotUpstreamTrace(c.Request.Context()).RequestID {
		return
	}
	transport, meta := req.Transport()
	if transport == "" || transport == database.TransportNative {
		return
	}
	input.Transport = transport
	if input.PluginMeta == "" {
		input.PluginMeta = meta
	}
	if p, ok := plugins.Default().Get(transport); ok {
		if endpoint := p.Describe().UpstreamEndpoint; endpoint != "" {
			input.UpstreamEndpoint = endpoint
		}
	}
}
