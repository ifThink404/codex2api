package proxy

import (
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const apiRelaySessionExemptContextKey = "api_relay_session_exempt"

// A window preflight has no model request to route. Exempt only an authenticated
// key whose entire authorized account pool is API relay, including temporarily
// unavailable accounts and both sides of fingerprint-based group routing.
func (handler *Handler) apiRelayOnlyWindowControlKey(request *gin.Context) bool {
	row := apiKeyRowFromContext(request)
	keyID := requestAPIKeyID(request)
	if handler == nil || handler.store == nil || row == nil || keyID <= 0 || row.ID != keyID {
		return false
	}
	found := false
	for _, account := range handler.store.Accounts() {
		if account == nil || !account.AllowsAPIKey(keyID) || !handler.store.APIKeyAllowsAccount(keyID, account) {
			continue
		}
		if !account.IsOpenAIResponsesAPI() {
			return false
		}
		found = true
	}
	return found
}

func apiRelaySessionExempt(request *gin.Context) bool {
	return request != nil && request.GetBool(apiRelaySessionExemptContextKey)
}

func apiRelaySessionAccountFilter(inner auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		return account != nil && account.IsOpenAIResponsesAPI() && (inner == nil || inner(account))
	}
}

func apiRelaySessionIdentity(identity requestSessionIdentity) requestSessionIdentity {
	identity.affinityID = firstNonEmptyString(identity.affinityID, identity.upstreamSeed)
	// Preserve root-window accounting, but not the native Codex-only extra
	// concurrency lease granted to protected internal requests.
	identity.protectedRelatedLease = false
	identity.apiRelayCapacityKey = capacityAwareSessionAffinityKey(identity, 0)
	// User-window exemptions must not change a verified parent relationship or
	// create a second account binding for the same root conversation.
	identity.requiresRootAccount = identity.requiresRootAccount && identity.relatedToRoot
	identity.unlinkedFallbackOnly = false
	identity.bypassWindowAccounting = true
	return identity
}

// Read the original owner before model filtering. In particular, native
// background requests may use models absent from the parent's public model list.
// Older relay bindings are only a fallback when the original root has no owner.
func (handler *Handler) apiRelayRootOwner(request *gin.Context, identity requestSessionIdentity) (int64, bool, error) {
	for _, original := range []string{identity.affinityID, identity.forkSourceAffinityID} {
		if original == "" {
			continue
		}
		key := sessionAffinityKey(original, requestAPIKeyID(request))
		entry, found, err := handler.readSessionContinuity(request.Request.Context(), hashRiskIdentity(key))
		if err != nil {
			return 0, false, err
		}
		if found {
			return entry.Record.AccountID, true, nil
		}
		if owner, found := handler.store.LiveSessionAccountID(key, time.Now()); found {
			return owner, true, nil
		}
		legacy := sessionAffinityKey("api-relay:"+original, requestAPIKeyID(request))
		for _, legacyKey := range []string{legacy, auth.SessionAccountingBypassAffinityKey(legacy)} {
			if owner, found := handler.store.LiveSessionAccountID(legacyKey, time.Now()); found {
				account := handler.store.FindByID(owner)
				filter := applyAffinityGroupRouting(request, identity, nil)
				if account != nil && account.IsOpenAIResponsesAPI() && account.AllowsAPIKey(requestAPIKeyID(request)) && handler.store.APIKeyAllowsAccount(requestAPIKeyID(request), account) && (filter == nil || filter(account)) {
					if restored, ok := handler.store.RestoreLegacyAPISession(legacyKey, key, account); ok {
						return restored, true, nil
					}
				}
			}
		}
	}
	return 0, false, nil
}

func (handler *Handler) configureAPIRelaySessionPolicy(request *gin.Context, body []byte, identity requestSessionIdentity) requestSessionIdentity {
	request.Set(apiRelaySessionExemptContextKey, false)
	if handler == nil || handler.store == nil || request.Request == nil || request.Request.URL == nil {
		return identity
	}
	handler.prepareChatGroupRouting(request, identity)
	if isResponsesWebSocketUpgradeRequest(request.Request) {
		return identity
	}
	path := request.Request.URL.Path
	if !strings.HasSuffix(path, "/responses") && !strings.HasSuffix(path, "/responses/compact") && !strings.HasSuffix(path, "/chat/completions") && !strings.HasSuffix(path, "/messages") {
		return identity
	}
	channel := requestUpstreamChannel(request)
	if channel != "" && channel != database.UpstreamChannelCodex {
		return identity
	}
	owner, ownerFound, ownerErr := handler.apiRelayRootOwner(request, identity)
	if ownerErr != nil {
		return identity
	}
	if ownerFound {
		recordUsageRootAccount(request, owner, true)
		account := handler.store.FindByID(owner)
		// Never detach from an existing native parent because only an API relay
		// survives the current request's ordinary model filter.
		if account == nil || !account.IsOpenAIResponsesAPI() {
			return identity
		}
		rememberAPIRelayDispatchScope(request, account)
		filter := applyAffinityGroupRouting(request, identity, nil)
		if !account.AllowsAPIKey(requestAPIKeyID(request)) || !handler.store.APIKeyAllowsAccount(requestAPIKeyID(request), account) || filter != nil && !filter(account) {
			return identity
		}
		request.Set(apiRelaySessionExemptContextKey, true)
		return apiRelaySessionIdentity(identity)
	}
	model := gjson.GetBytes(body, "model").String()
	originalModel := trustedRequestedModel(request, model)
	if strings.HasSuffix(path, "/messages") {
		routingBody := handler.resolveMessagesRoutingBodyForRequest(request, body, originalModel, handler.supportedModelIDs(request.Request.Context()))
		model = effectiveRequestModel(routingBody, model)
	}
	filter := applyAffinityGroupRouting(request, identity, sessionModelSupportFilter(originalModel, model, isCompactUsageEndpoint(path)))
	apiKeyID := requestAPIKeyID(request)
	allowed := func(account *auth.Account) bool {
		return account != nil && account.AllowsAPIKey(apiKeyID) && handler.store.APIKeyAllowsAccount(apiKeyID, account) && filter(account)
	}
	foundAPI, foundOther := false, false
	for _, account := range handler.store.Accounts() {
		if !allowed(account) {
			continue
		}
		if account.IsOpenAIResponsesAPI() {
			foundAPI = true
		} else {
			foundOther = true
		}
	}
	if !foundAPI {
		return identity
	}
	if foundOther {
		_, number, known, invalid := parseContinuityWindow(request.Request.Header, body, false)
		continuityBlocked := handler.promptFilterConfigForRequest(request).Advanced.Risk.SessionContinuityMode == "enforce" && (invalid != "" || known && number > 0)
		if identity.requiresRootAccount || identity.forkSourceAffinityID != "" || !requestIsSessionCompaction(request, body) && !continuityBlocked {
			return identity
		}
	}
	request.Set(apiRelaySessionExemptContextKey, true)
	return apiRelaySessionIdentity(identity)
}
