package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

// Transport plugin glue. Every handler site calls exactly one of these, so
// the plugin framework (proxy/plugins) stays out of the handler control flow.

const contextTransportPluginRequest = "transport_plugin_request"

// transportPluginHost is the Request.Host handle for plugins compiled into
// this package (BPS): the handler and the inbound gin context.
type transportPluginHost struct {
	handler *Handler
	c       *gin.Context
}

func transportPluginHostOf(req *plugins.Request) *transportPluginHost {
	if req == nil {
		return nil
	}
	host, _ := req.Host.(*transportPluginHost)
	return host
}

// transportPluginRequest returns the per-inbound-request plugin context,
// creating it on first use and attaching it to the request context. A
// downstream WebSocket reuses one gin.Context for many turns but resets the
// request trace per turn, so a changed request ID starts a fresh request.
func (h *Handler) transportPluginRequest(c *gin.Context, kind plugins.RequestKind, body []byte, model string) *plugins.Request {
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
	req.Model = model
	req.Host = &transportPluginHost{handler: h, c: c}
	c.Set(contextTransportPluginRequest, req)
	c.Request = c.Request.WithContext(plugins.WithRequest(c.Request.Context(), req))
	return req
}

// resolveTransportPlugin decides, once per attempt, whether a transport plugin
// serves it. nil means the native path; relay accounts always stay native.
// The decision is recorded on the request so usage logs attribute the attempt
// to its transport.
func (h *Handler) resolveTransportPlugin(c *gin.Context, account *auth.Account, model string, kind plugins.RequestKind, body []byte) *plugins.Route {
	registry := plugins.Default()
	if len(registry.Plugins()) == 0 || c == nil || c.Request == nil || account == nil {
		return nil
	}
	req := h.transportPluginRequest(c, kind, body, model)
	if account.IsRelayStyle() {
		registry.MarkNative(req)
		return nil
	}
	return registry.Resolve(c.Request.Context(), req, account, model, kind)
}

// applyTransportPluginFilter binds the request to the plugins and adds their
// pin and Admissible rules to the scheduler account filter.
func (h *Handler) applyTransportPluginFilter(c *gin.Context, model string, kind plugins.RequestKind, body []byte, filter auth.AccountFilter) auth.AccountFilter {
	registry := plugins.Default()
	if len(registry.Plugins()) == 0 {
		return filter
	}
	req := h.transportPluginRequest(c, kind, body, model)
	return registry.AccountFilter(c.Request.Context(), req, kind, model, filter)
}

// nextAccountWithTransportPlugins is nextAccountForSessionWithDispatchGuard
// with the plugins' soft account preference tried first.
func (h *Handler) nextAccountWithTransportPlugins(ctx context.Context, affinityKey string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) (*auth.Account, string, auth.SessionAffinityGuard) {
	req := plugins.RequestFromContext(ctx)
	registry := plugins.Default()
	if req == nil || len(registry.Plugins()) == 0 {
		return h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, filter, policy)
	}
	if id := registry.PreferredAccount(ctx, req, req.Model); id > 0 && !exclude[id] {
		if account := h.store.TakePreferredAccountWithDispatch(id, apiKeyID, exclude, filter, policy); account != nil {
			registry.AccountSelected(ctx, req, account, req.Model)
			return account, account.GetProxyURL(), auth.SessionAffinityGuard{}
		}
	}
	if prefer := registry.PreferenceFilter(ctx, req, req.Model); prefer != nil {
		preferred := func(account *auth.Account) bool { return (filter == nil || filter(account)) && prefer(account) }
		if account, proxyURL, guard := h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, preferred, policy); account != nil {
			registry.AccountSelected(ctx, req, account, req.Model)
			return account, proxyURL, guard
		}
	}
	account, proxyURL, guard := h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, filter, policy)
	if account != nil {
		registry.AccountSelected(ctx, req, account, req.Model)
	}
	return account, proxyURL, guard
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
	if kind := req.UsageErrorKind(transport); kind != "" {
		input.UpstreamErrorKind = kind
	}
	if message := req.UsageErrorMessage(); message != "" && input.ErrorMessage != "" {
		input.ErrorMessage = message
	}
	if endpoint := req.UsageUpstreamEndpoint(transport); endpoint != "" {
		input.UpstreamEndpoint = endpoint
	} else if p, ok := plugins.Default().Get(transport); ok {
		if endpoint := p.Describe().UpstreamEndpoint; endpoint != "" {
			input.UpstreamEndpoint = endpoint
		}
	}
	input.ViaWebsocket = false
}

// Native account health for transport plugins. A plugin implementing
// plugins.NativeHealthPolicy can keep its transport-level failures (4xx/422,
// upload errors, 5xx, stream failures, its own rate limits) out of the native
// health score and cooldowns. Account-level signals always reach native
// health, because they describe the account, not the transport. The handler
// sites call these wrappers instead of the helpers they delegate to; for
// native attempts they are exact pass-throughs.

// transportPluginSparesNativeHealth reports whether the attempt in flight on c
// is served by a plugin whose policy spares native health.
func transportPluginSparesNativeHealth(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	served := plugins.RequestFromContext(c.Request.Context()).Served()
	if served == "" || served == database.TransportNative {
		return false
	}
	p, ok := plugins.Default().Get(served)
	if !ok {
		return false
	}
	policy, ok := p.(plugins.NativeHealthPolicy)
	return ok && policy.SparesNativeHealth()
}

// nativeHealthAccountSignal reports failures that describe the account
// itself: a revoked or invalid credential (401, which also disables the
// account and drives token refresh), a deactivated workspace, a deleted agent
// runtime, or a payment requirement. These reach native health even when a
// plugin spares its failures.
func nativeHealthAccountSignal(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired:
		return true
	}
	return IsDeactivatedWorkspaceError(body) || IsAgentRuntimeDeletedError(body)
}

// nativeHealthAccountFailureKind is nativeHealthAccountSignal for the failure
// kinds ReportRequestFailure records.
func nativeHealthAccountFailureKind(kind string) bool {
	return kind == "unauthorized"
}

func (h *Handler) reportAttemptFailure(c *gin.Context, account *auth.Account, kind string, d time.Duration) {
	if transportPluginSparesNativeHealth(c) && !nativeHealthAccountFailureKind(kind) {
		return
	}
	h.store.ReportRequestFailure(account, kind, d)
}

func (h *Handler) applyAttemptCooldown(c *gin.Context, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	if transportPluginSparesNativeHealth(c) && !nativeHealthAccountSignal(statusCode, body) {
		return codex429Decision{}
	}
	return h.applyCooldownForModel(account, statusCode, body, resp, model)
}

func (h *Handler) applyAttemptResponseFailedCooldown(c *gin.Context, account *auth.Account, payload []byte, resp *http.Response, model string) codex429Decision {
	if transportPluginSparesNativeHealth(c) && !nativeHealthAccountSignal(responseFailedStatusCode(payload), responseFailedErrorBody(payload)) {
		return codex429Decision{}
	}
	return h.applyResponseFailedCooldown(account, payload, resp, model)
}

func (h *Handler) reportAttemptOutcomeFailure(c *gin.Context, account *auth.Account, outcome streamOutcome, d time.Duration) {
	if transportPluginSparesNativeHealth(c) && !nativeHealthAccountFailureKind(outcome.failureKind) &&
		!nativeHealthAccountSignal(outcome.logStatusCode, responseFailedErrorBody(outcome.failurePayload)) {
		return
	}
	h.reportStreamOutcomeFailure(account, outcome, d)
}
