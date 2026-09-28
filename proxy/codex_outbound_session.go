package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func codexOutboundSessionMode() string {
	if currentCodexSessionRecoveryPolicy().Failover {
		return "account"
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_OUTBOUND_SESSION_MODE"))) {
	case "legacy", "off":
		return "legacy"
	case "observe":
		return "observe"
	case "account":
		return "account"
	default:
		return "preserve"
	}
}

func NewCodexTransportFingerprint(account *auth.Account, headers http.Header, body []byte, upstreamSessionID string, contexts ...context.Context) *CodexFingerprint {
	fingerprint := NewCodexFingerprint(account, headers, body)
	mode := codexOutboundSessionMode()
	if len(contexts) > 0 && outboundEpochFromContext(contexts[0]).identityKey() != "" {
		mode = "account"
	}
	// Relaxed recovery and new default sessions prefer persistent account
	// identities only when the account and store can actually provide them.
	// Legacy accounts without an upstream ID keep their existing initial route;
	// a committed migration above always requires account identities.
	if (currentCodexSessionRecoveryPolicy().Relaxed || strings.TrimSpace(os.Getenv("CODEX_OUTBOUND_SESSION_MODE")) == "") && len(contexts) > 0 && contexts[0] != nil && account != nil && !account.IsRelayStyle() && account.EffectiveAccountID() != "" {
		if _, ok := contexts[0].Value(codexIdentityClaimerContextKey{}).(CodexIdentityStore); ok {
			mode = "account"
		}
	}
	if account == nil || account.IsRelayStyle() || (mode != "preserve" && mode != "account") {
		return fingerprint
	}
	fingerprint.preserveSessionIDs = true
	fingerprint.accountIdentityRequested = mode == "account"
	if len(contexts) > 0 {
		fingerprint.completeRelaxedSessionIdentity(contexts[0])
	}
	body = fingerprint.sessionIdentityFallback.completeBody(NormalizeCodexRequestMetadata(body))
	fingerprint.identityValues = codexTransportIdentityValues(fingerprint.headers, body)
	fingerprint.accountIdentityInputs = codexAccountIdentityInputs(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountRequestIdentityInputs = codexAccountRequestIdentityInputs(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountTurnIdentityInputs = codexAccountTurnIdentityInputs(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountIdentityReferences = codexAccountIdentityReferences(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountWindowInputs, fingerprint.accountWindowInputError = codexAccountWindowIdentities(fingerprint.headers, NormalizeCodexRequestMetadata(body), CurrentRuntimeSettings().CodexForkAccountFallbackEnabled)
	if fingerprint.ids != nil {
		fingerprint.ids.mode = auth.CodexFingerprintModeDevice
		fingerprint.ids.sessionID = ""
		fingerprint.ids.threadID = ""
		fingerprint.ids.windowID = ""
		fingerprint.ids.clientRequestID = ""
		fingerprint.ids.lineageValues = nil
	}
	return fingerprint
}

func (fingerprint *CodexFingerprint) PreservesSessionIdentity() bool {
	return fingerprint != nil && fingerprint.preserveSessionIDs
}

func (fingerprint *CodexFingerprint) ApplySessionHeaders(outbound http.Header) {
	if !fingerprint.PreservesSessionIdentity() || outbound == nil {
		return
	}
	for _, name := range []string{codexSessionIDHeader, codexLegacySessionIDHeader, codexConversationIDHeader, "Conversation-Id"} {
		outbound.Del(name)
	}
	sessionID := fingerprint.headers.Get(codexSessionIDHeader)
	if sessionID == "" {
		sessionID = fingerprint.headers.Get(codexLegacySessionIDHeader)
	}
	if sessionID != "" {
		outbound.Set(codexSessionIDHeader, sessionID)
	}
	for _, name := range []string{codexThreadIDHeader, codexClientRequestIDHeader, codexWindowIDHeader, codexParentThreadIDHeader, "X-Codex-Forked-From-Thread-Id", "X-OpenAI-Subagent", "X-OpenAI-Memgen-Request", codexTurnMetadataHeader} {
		outbound.Del(name)
		if value := fingerprint.headers.Get(name); value != "" {
			outbound.Set(name, value)
		}
	}
	if outbound.Get(codexClientRequestIDHeader) == "" && outbound.Get(codexThreadIDHeader) != "" {
		outbound.Set(codexClientRequestIDHeader, outbound.Get(codexThreadIDHeader))
	}
	applyCodexFingerprintHeaders(outbound, fingerprint.ids, fingerprint.headers)
	if fingerprint.accountIdentity != nil {
		outbound.Set("Chatgpt-Account-Id", fingerprint.accountIdentity.account)
	}
}

func ScopeCodexPromptCacheKey(ctx context.Context, cacheKey string) string {
	if owner := verifiedTransportUser(ctx); owner != "" && strings.TrimSpace(cacheKey) != "" {
		return DeriveStableSessionUUIDv7("codex-cache-user-v2:" + owner + ":" + cacheKey)
	}
	return cacheKey
}

// ResolveCodexPromptCacheSeed preserves explicit client cache namespaces (for
// example guardian:root and guardian-v2:root). The usual session-ID default
// keeps its existing seed so ordinary main/subagent cache sharing is unchanged.
// This is a seed only: caller/user and account scoping still run afterwards.
func ResolveCodexPromptCacheSeed(body []byte, headers http.Header, upstreamSessionID string) string {
	explicit := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	if IsStatelessWebsocketSessionID(upstreamSessionID) {
		upstreamSessionID = ""
	}
	if explicit == "" {
		return upstreamSessionID
	}
	if upstreamSessionID == "" {
		return explicit
	}
	metadata := codexTurnMetadata(body, headers)
	root := metadata.Get("session_id").String()
	if root == "" {
		root = gjson.GetBytes(body, "client_metadata.session_id").String()
	}
	if root == "" {
		root = headers.Get(codexSessionIDHeader)
	}
	if root == "" {
		root = headers.Get(codexLegacySessionIDHeader)
	}
	if explicit == root || explicit == upstreamSessionID {
		return upstreamSessionID
	}
	seed, _ := json.Marshal([]string{"codex-explicit-cache-v1", upstreamSessionID, explicit})
	return DeriveStableSessionUUIDv7(string(seed))
}

func codexTransportIdentityValues(headers http.Header, body []byte) []string {
	values := []string{headers.Get(codexSessionIDHeader), headers.Get(codexLegacySessionIDHeader), headers.Get(codexThreadIDHeader), headers.Get(codexParentThreadIDHeader), headers.Get("X-Codex-Forked-From-Thread-Id")}
	metadata := gjson.Parse(headers.Get(codexTurnMetadataHeader))
	for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id"} {
		if value := metadata.Get(field); value.Type == gjson.String {
			values = append(values, value.String())
		}
	}
	for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "x-codex-parent-thread-id", "x_codex_parent_thread_id", "x-codex-forked-from-thread-id", "x_codex_forked_from_thread_id"} {
		if value := gjson.GetBytes(body, "client_metadata."+field); value.Type == gjson.String {
			values = append(values, value.String())
		}
	}
	return values
}
