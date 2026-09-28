package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Raw transport is a property of the selected route, not permission to replace
// an existing ordinary owner or preempt a native pool in the same group.
func (h *Handler) rawRelayRouteOwner(c *gin.Context, identity requestSessionIdentity, model string, filter auth.AccountFilter) (*auth.Account, string, bool, bool) {
	ownerID, found, err := h.apiRelayRootOwner(c, identity)
	if err != nil {
		return nil, "", false, false // ordinary handler retains its fail-closed checks
	}
	key := ""
	if identity.stableIdentity && identity.affinityID != "" {
		key = sessionAffinityKey("raw-api:"+identity.affinityID, requestAPIKeyID(c))
	}
	if found {
		owner := h.store.FindByID(ownerID)
		if owner == nil || !owner.OpenAIRawPassthroughEnabled() {
			return nil, key, false, false
		}
	}
	rawOwner := false
	if key != "" {
		if rawID, exists := h.store.LiveSessionAccountID(key, time.Now()); exists {
			// A successful raw failover supersedes an older API binding, but
			// never supersedes the native ownership checked above.
			ownerID, found, rawOwner = rawID, true, true
		}
	}
	if found {
		owner := h.store.FindByID(ownerID)
		if owner == nil || !owner.OpenAIRawPassthroughEnabled() {
			return nil, key, rawOwner, rawOwner
		}
		return owner, key, true, false
	}
	modelFilter := sessionModelSupportFilter(model, model, isCompactUsageEndpoint(c.Request.URL.Path))
	for _, account := range h.store.Accounts() {
		if !account.IsRelayStyle() && account.AllowsAPIKey(requestAPIKeyID(c)) && h.store.APIKeyAllowsAccount(requestAPIKeyID(c), account) && (filter == nil || filter(account)) && modelFilter(account) {
			return nil, key, false, false
		}
	}
	return nil, key, true, false
}

// Opaque continuation handles cannot be copied to another upstream credential.
// Keep them intact and return the owner's response instead of rewriting input.
func rawRelayCanSwitch(c *gin.Context, body []byte) bool {
	if gjson.GetBytes(body, "previous_response_id").String() != "" || gjson.GetBytes(body, "conversation").Exists() || codexTurnContinuationToken(c.Request.Header, body) != "" {
		return false
	}
	for _, field := range []string{"input", "messages"} {
		for _, item := range gjson.GetBytes(body, field).Array() {
			if rawRelayHasAccountReference(item, 0) {
				return false
			}
		}
	}
	return true
}

// Inspect protocol content only, never arbitrary tool arguments or text that
// happens to contain a file_id. Chat file wrappers and Messages file sources
// carry the same account-bound references as Responses input_file items.
func rawRelayHasAccountReference(item gjson.Result, depth int) bool {
	if !item.IsObject() {
		return false
	}
	if depth >= 16 {
		return true // Unusually deep content cannot be safely replayed unchanged.
	}
	if item.Get("file_id").String() != "" || item.Get("encrypted_content").String() != "" || item.Get("type").String() == "item_reference" {
		return true
	}
	if kind := item.Get("type").String(); kind == "redacted_thinking" || kind == "thinking" && item.Get("signature").String() != "" {
		return true
	}
	for _, field := range []string{"file", "source", "image_url"} {
		if item.Get(field).Get("file_id").String() != "" {
			return true
		}
	}
	for _, content := range item.Get("content").Array() {
		if rawRelayHasAccountReference(content, depth+1) {
			return true
		}
	}
	return false
}

func (h *Handler) dispatchRawRelay(c *gin.Context, body, routingBody []byte, model string, owner *auth.Account, key string, filter auth.AccountFilter) {
	keyID := requestAPIKeyID(c)
	policy := auth.DispatchPolicyStandard.WithModel(model)
	trace := selectionTraceForRequest(c)
	source := owner
	var groups map[int64]struct{}
	if source != nil {
		groups = apiRelayConfiguredSwitchGroups(c, source)
	}
	canSwitch := rawRelayCanSwitch(c, routingBody)
	canRetry := func() bool {
		return canSwitch && (CurrentRuntimeSettings().CodexForkAccountFallbackEnabled || len(groups) > 0)
	}
	excluded := map[int64]bool{}
	selectAccount := func() *auth.Account {
		if c.Request.Context().Err() != nil {
			return nil
		}
		eligible := func(a *auth.Account) bool {
			if filter != nil && !filter(a) {
				return false
			}
			if source == nil || a.ID() == source.ID() {
				return true
			}
			if !canSwitch {
				return false
			}
			if CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
				return true
			}
			currentGroups := apiRelayConfiguredSwitchGroups(c, source)
			for id := range currentGroups {
				if _, exists := groups[id]; !exists {
					delete(currentGroups, id)
				}
			}
			return len(currentGroups) > 0 && a.InAnyGroup(currentGroups)
		}
		// Reuse an available owner before trying another eligible API account.
		if source != nil && !excluded[source.ID()] {
			if a := h.store.NextExcludingWithDispatch(keyID, excluded, func(a *auth.Account) bool { return a.ID() == source.ID() && eligible(a) }, policy, trace); a != nil {
				return a
			}
		}
		return h.store.NextExcludingWithDispatch(keyID, excluded, eligible, policy, trace)
	}
	account := selectAccount()
	if account == nil {
		if msg := scopeBudgetExhaustedMessage(c); msg != "" {
			SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
		} else {
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "No eligible raw API account is available", api.ErrorTypeServer), http.StatusServiceUnavailable)
		}
		return
	}
	defer func() { h.store.Release(account) }()
	h.AcquireAPIKeyScopeConcurrency(c, account)
	if err := ConsumeAPIKeyModelRequestQuota(c.Request.Context(), model); err != nil {
		ErrorToGinResponse(c, err)
		return
	}
	if source == nil {
		source, groups = account, apiRelayConfiguredSwitchGroups(c, account)
	}
	generalRetries, rateRetries := 0, 0
	// One attempt per eligible account. Continuous retry never widens the pool
	// or cycles indefinitely, and successful streams are never buffered/replayed.
	for attempt := 1; ; attempt++ {
		attemptStart := time.Now()
		diagnostic := &rawRelayDiagnostic{Enabled: true, RequestBytes: len(body), UsageSource: "not_observed"}
		usageRequestDiagnosticState(c).RawPassthrough = diagnostic
		input := &database.UsageLogInput{AccountID: account.ID(), Endpoint: c.Request.URL.Path, InboundEndpoint: c.Request.URL.Path, UpstreamEndpoint: c.Request.URL.Path, Model: model, EffectiveModel: model, Stream: gjson.GetBytes(routingBody, "stream").Bool(), AttemptIndex: attempt, IsRetryAttempt: attempt > 1, ReasoningEffort: extractReasoningEffort(routingBody)}
		logAttempt := func() {
			input.DurationMs = int(time.Since(attemptStart).Milliseconds())
			if h.db != nil {
				h.logUsageForRequest(c, input)
			}
		}
		resp, err := h.sendRawRelay(c, account, body)
		var failureBody []byte
		retry := false
		if err != nil {
			input.StatusCode, input.ErrorMessage = http.StatusBadGateway, "Raw API transport failed"
			retry = shouldRetryRequestError(err, &generalRetries, h.getMaxRetries(), database.ContinuousRetryPolicy{})
		} else {
			input.StatusCode = resp.StatusCode
			encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
			if resp.StatusCode >= 400 && canRetry() && (encoding == "" || encoding == "identity") {
				// Inspect a bounded error copy only. Reattach every consumed byte
				// when no retry is made, including oversized/partial error bodies.
				failureBody, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
				var tail io.Reader = resp.Body
				if err != nil {
					tail = rawRelayReadFailure{err}
				}
				resp.Body = &rawRelayReplayBody{Reader: io.MultiReader(bytes.NewReader(failureBody), tail), Closer: resp.Body}
				if err == nil && len(failureBody) <= 1<<20 {
					retry = h.shouldRetryUpstreamHTTPStatus(resp.StatusCode, failureBody, &generalRetries, &rateRetries, h.getMaxRetries(), h.getMaxRateLimitRetries(), database.ContinuousRetryPolicy{})
				}
			}
		}
		if retry && canRetry() && c.Request.Context().Err() == nil && !c.Writer.Written() {
			if resp != nil && h.rateLimitFailureDisposition(resp.StatusCode, failureBody, true, database.ContinuousRetryPolicy{}).retrySameAccount {
				resp.Body.Close()
				observer := &rawRelayUsageObserver{}
				observer.Write(failureBody)
				observer.finish()
				applyRawRelayObservation(input, diagnostic, observer, routingBody)
				logAttempt()
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), rateRetries, h.getMaxRateLimitRetries(), resp) {
					return
				}
				continue
			}
			excluded[account.ID()] = true
			if next := selectAccount(); next != nil {
				if resp != nil {
					resp.Body.Close()
					observer := &rawRelayUsageObserver{}
					observer.Write(failureBody)
					observer.finish()
					applyRawRelayObservation(input, diagnostic, observer, routingBody)
				}
				diagnostic.RetryAccountID = next.ID()
				logAttempt()
				h.store.Release(account)
				h.ReleaseAPIKeyScopeConcurrency(c)
				account = next
				h.AcquireAPIKeyScopeConcurrency(c, account)
				continue
			}
		}
		if resp == nil {
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, input.ErrorMessage, api.ErrorTypeServer), http.StatusBadGateway)
		} else {
			// Bind only successful headers. The next user request prefers this
			// account; Codex persistent bindings are never created or overwritten.
			if resp.StatusCode < 300 && resp.StatusCode >= 200 && key != "" {
				h.store.BindSessionAffinity(key, account, account.GetProxyURL())
			}
			h.writeRawRelayResponse(c, resp, routingBody, input, diagnostic, attemptStart)
			resp.Body.Close()
		}
		logAttempt()
		return
	}
}

type rawRelayReplayBody struct {
	io.Reader
	io.Closer
}

type rawRelayReadFailure struct{ err error }

func (r rawRelayReadFailure) Read([]byte) (int, error) { return 0, r.err }

func (h *Handler) sendRawRelay(c *gin.Context, account *auth.Account, body []byte) (*http.Response, error) {
	proxyURL, usable := h.store.ResolveUsableProxyForAccount(account)
	if !usable {
		return nil, fmt.Errorf("raw API egress unavailable")
	}
	baseURL, credential := account.OpenAIResponsesCredentials()
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, c.Request.URL.EscapedPath())
	if c.Request.URL.RawQuery != "" {
		endpoint += "?" + c.Request.URL.RawQuery
	}
	client, err := rawRelayClient(account, proxyURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = rawRelayRequestHeaders(c.Request.Header, account, credential)
	recordTrace := beginUpstreamTrace(req.Context(), account, proxyURL, false)
	UpstreamTransportObserver(req.Context()).Endpoint(endpoint)
	resp, err := client.Do(req)
	recordTrace(resp)
	if err != nil {
		UpstreamTransportObserver(req.Context()).Failure("upstream_transport", "request", 0)
	}
	return resp, err
}
