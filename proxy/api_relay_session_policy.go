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
const apiRelayAffinitySourceContextKey = "api_relay_affinity_source"

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

func (handler *Handler) seedAPIRelayAffinity(request *gin.Context, identity requestSessionIdentity, key string) {
	source := request.GetString(apiRelayAffinitySourceContextKey)
	if source == "" || source == key {
		return
	}
	if _, found := handler.store.LiveSessionAccountID(key, time.Now()); found {
		return
	}
	owner, found := handler.store.LiveSessionAccountID(source, time.Now())
	account := handler.store.FindByID(owner)
	apiKeyID := requestAPIKeyID(request)
	if found && account != nil && account.AllowsAPIKey(apiKeyID) && handler.store.APIKeyAllowsAccount(apiKeyID, account) && applyAffinityGroupRouting(request, identity, nil)(account) {
		handler.store.BindSessionAffinity(key, account, account.GetProxyURL())
	}
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
	identity.affinityID = "api-relay:" + firstNonEmptyString(identity.affinityID, identity.upstreamSeed)
	// Preserve root-window accounting, but not the native Codex-only extra
	// concurrency lease granted to protected internal requests.
	identity.protectedRelatedLease = false
	identity.apiRelayCapacityKey = capacityAwareSessionAffinityKey(identity, 0)
	identity.relatedToRoot, identity.ownsRootBinding, identity.requiresRootAccount = false, false, false
	identity.unlinkedFallbackOnly = false
	identity.forkSourceAffinityID = ""
	identity.bypassWindowAccounting = true
	return identity
}

func (handler *Handler) configureAPIRelaySessionPolicy(request *gin.Context, body []byte, identity requestSessionIdentity) requestSessionIdentity {
	request.Set(apiRelaySessionExemptContextKey, false)
	request.Set(apiRelayAffinitySourceContextKey, "")
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
		apiOwner := false
		ownerFound := false
		for _, original := range []string{identity.affinityID, identity.forkSourceAffinityID} {
			if original == "" {
				continue
			}
			key := sessionAffinityKey(original, apiKeyID)
			entry, found, err := handler.readSessionContinuity(request.Request.Context(), hashRiskIdentity(key))
			if err != nil {
				return identity
			}
			owner := entry.Record.AccountID
			if !found {
				owner, _ = handler.store.LiveSessionAccountID(key, time.Now())
				if owner == 0 {
					sourceIdentity := identity
					sourceIdentity.affinityID = original
					owner, _ = handler.store.LiveSessionAccountID(capacityAwareSessionAffinityKey(apiRelaySessionIdentity(sourceIdentity), apiKeyID), time.Now())
				}
			}
			if owner == 0 {
				continue
			}
			account := handler.store.FindByID(owner)
			ownerFound = true
			apiOwner = allowed(account) && account.IsOpenAIResponsesAPI()
			break
		}
		if !apiOwner {
			if ownerFound {
				return identity
			}
			_, number, known, invalid := parseContinuityWindow(request.Request.Header, body, false)
			continuityBlocked := handler.promptFilterConfigForRequest(request).Advanced.Risk.SessionContinuityMode == "enforce" && (invalid != "" || known && number > 0)
			if !identity.requiresRootAccount && identity.forkSourceAffinityID == "" && !requestRequiresCompactionOwner(request, body) && !continuityBlocked {
				return identity
			}
		}
	}
	request.Set(apiRelaySessionExemptContextKey, true)
	request.Set(apiRelayAffinitySourceContextKey, sessionAffinityKey(identity.affinityID, apiKeyID))
	return apiRelaySessionIdentity(identity)
}
