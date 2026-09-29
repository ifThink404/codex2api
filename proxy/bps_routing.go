package proxy

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/plugins"
	"github.com/tidwall/gjson"
)

// BPS routing policy. Native Codex traffic gets accounts marked as degraded
// upstream, so a BPS account never spills onto the native transport unless
// its native route is explicitly enabled (codex_native_enabled=true). When
// BPS cannot serve a request on an account (the model is not available on
// BPS, the account is cooling or over budget, a marked conversation), the
// account is vetoed and the request goes to another account. Only when no
// other account could serve it is the account admitted anyway, and the
// attempt fails fast without contacting any upstream: 503 bps_unavailable
// (400 for a model not on BPS; a full account waits briefly for a slot).

const (
	bpsConversationBlockTTL = 30 * time.Minute
	bpsConversationBlockKey = "conversation:"
)

// bpsNativeExplicit reports whether account's native route is explicitly on
// for model (native is never implied for a BPS account).
func bpsNativeExplicit(account *auth.Account, model string, related bool) bool {
	return account.CodexRouteAllows("native", model, related, true)
}

// blockReason is why BPS cannot serve this request on account, or "".
func (s *bpsRequest) blockReason(ctx context.Context, account *auth.Account, model string) string {
	related := s != nil && s.related
	switch {
	case s.policyBlocked():
		return BPSPolicyBlockedKind
	case s.accountExcluded(account):
		return "bps_account_refused"
	}
	if record, cooling := bpsAccountCooling(ctx, s.cache(), account.ID()); cooling {
		return record.Reason
	}
	if routeBreakerOpen(ctx, s.cache(), account, RouteBPS) {
		return BPSDegradedReason
	}
	if !bpsRouteAllows(ctx, s.cache(), account, model, related) {
		return BPSModelUnavailable
	}
	if bpsBudgetExhausted(ctx, s.cache(), account) {
		return BPSBudgetExhaustedReason
	}
	if bpsConcurrencyFull(account) {
		return BPSConcurrencyFullReason
	}
	return ""
}

func (s *bpsRequest) policyBlocked() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}

// markConversationBlocked marks the request's conversation so its later turns
// avoid BPS for 30 minutes (bps_policy_conversation_mark). The current
// request still fails over to another BPS account.
func (s *bpsRequest) markConversationBlocked(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	keys := append([]string(nil), s.conversationKeys...)
	s.mu.Unlock()
	now := time.Now()
	for _, key := range keys {
		bpsAccountStateStore.update(ctx, s.cache(), key, now, func(r *bpsAccountRecord) {
			r.Until, r.Reason = now.Add(bpsConversationBlockTTL), BPSPolicyBlockedKind
		})
	}
	if len(keys) > 0 {
		log.Printf("[bps] conversation blocked by the BPS usage policy; its turns avoid BPS until %s", now.Add(bpsConversationBlockTTL).Format(time.RFC3339))
	}
}

// bpsConversationKeys identify the conversation of a request (explicit
// session, prompt cache key, turn-metadata session, inferred session), scoped
// to the caller's API key.
func bpsConversationKeys(req *plugins.Request, inferred *inferredBPSSession) []string {
	if req == nil {
		return nil
	}
	var ids []string
	add := func(id string) {
		if id = strings.TrimSpace(id); id != "" && len(id) <= 512 {
			ids = append(ids, id)
		}
	}
	add(ResolveExplicitSessionID(req.Header, req.Body))
	add(gjson.GetBytes(req.Body, "prompt_cache_key").String())
	add(gjson.Get(CodexRequestMetadataHeaders(req.Header, req.Body).Get(codexTurnMetadataHeader), "session_id").String())
	if inferred != nil {
		add(inferred.seed)
	}
	seen := map[string]bool{}
	var keys []string
	for _, id := range ids {
		key := bpsConversationBlockKey + codexIdentityDigest("bps-policy-conversation-v1", strconv.FormatInt(req.APIKeyID, 10), id)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

// conversationBlocked reports whether a key of this conversation is marked.
func (s *bpsRequest) conversationBlocked(ctx context.Context) bool {
	now := time.Now()
	for _, key := range s.conversationKeys {
		if bpsAccountStateStore.load(ctx, s.cache(), key, now).active(now) {
			return true
		}
	}
	return false
}

// servableElsewhere reports whether an account other than account could take
// this request: a Codex-channel account that is not BPS-enabled, or a BPS
// account BPS or its explicit native route can serve. Cached per model.
func (s *bpsRequest) servableElsewhere(ctx context.Context, account *auth.Account, model string) bool {
	if s == nil || s.handler == nil || s.handler.store == nil {
		return true
	}
	s.mu.Lock()
	cached, ok := s.servable[model]
	s.mu.Unlock()
	if ok {
		return cached[account.ID()]
	}
	p, _ := plugins.Default().Get(BPSPluginID)
	var servers []int64
	for _, candidate := range s.handler.store.Accounts() {
		switch strings.ToLower(strings.TrimSpace(candidate.UpstreamType)) {
		case auth.UpstreamClaude, auth.UpstreamGrok, auth.UpstreamAntigravity:
			continue
		}
		if atomic.LoadInt32(&candidate.Disabled) == 1 || s.accountExcluded(candidate) {
			continue
		}
		if p == nil || !plugins.Default().EnabledFor(p, candidate) || s.blockReason(ctx, candidate, model) == "" || bpsNativeExplicit(candidate, model, s.related) {
			servers = append(servers, candidate.ID())
		}
	}
	// elsewhere[id] is true when some account other than id can serve.
	elsewhere := make(map[int64]bool, len(servers))
	for _, candidate := range s.handler.store.Accounts() {
		for _, id := range servers {
			if id != candidate.ID() {
				elsewhere[candidate.ID()] = true
				break
			}
		}
	}
	s.mu.Lock()
	if s.servable == nil {
		s.servable = map[string]map[int64]bool{}
	}
	s.servable[model] = elsewhere
	s.mu.Unlock()
	return elsewhere[account.ID()]
}

// bpsRefusal is the client error of an attempt admitted only because no
// other account could serve the request.
func bpsRefusal(reason, model string, until time.Time) *Error {
	switch reason {
	case BPSModelUnavailable:
		return bpsError(http.StatusBadRequest, "bps_model_unavailable", "model %s is not available on BPS for this account", model)
	case BPSConcurrencyFullReason:
		return bpsError(http.StatusTooManyRequests, "bps_concurrency_limited", "every BPS account is at its concurrency limit; retry shortly")
	}
	// Cooling, blocked or over budget: no account is contacted.
	if !until.IsZero() && until.After(time.Now()) {
		return bpsError(http.StatusServiceUnavailable, "bps_unavailable", "no BPS account is available right now (%s until %s)", reason, until.UTC().Format(time.RFC3339))
	}
	return bpsError(http.StatusServiceUnavailable, "bps_unavailable", "no BPS account is available right now (%s)", reason)
}
