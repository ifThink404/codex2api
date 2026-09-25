package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type inferredBPSSessionKey struct{}

// This is a cache/session hint, never proof of ownership or a client identity.
// Keep it outside requestSessionIdentity so it cannot grant a root/window lease.
type inferredBPSSession struct {
	seed       string
	diagnostic inferredBPSSessionDiagnostic
}

type inferredBPSSessionDiagnostic struct {
	Result       string `json:"result"`
	Reason       string `json:"reason,omitempty"`
	Source       string `json:"source,omitempty"`
	DeviceSource string `json:"device_source,omitempty"`
	UAIncluded   bool   `json:"ua_included"`
	Heuristic    bool   `json:"heuristic"`
}

func bindInferredBPSSession(c *gin.Context, body []byte, identity requestSessionIdentity, root requestRootSessionIdentity, policy verifiedNewAPIPolicyContext, verified bool) {
	state := &inferredBPSSession{diagnostic: inferredBPSSessionDiagnostic{Result: "skipped"}}
	// Reset on each WebSocket frame as well as each HTTP request.
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), inferredBPSSessionKey{}, state))
	switch {
	case root.conflict:
		state.diagnostic.Reason = "identity_conflict"
		return
	case identity.explicitUpstreamID != "" || root.stable || identity.hasDownstreamAffinity:
		state.diagnostic.Reason = "explicit_identity"
		return
	case root.related || identity.requiresRootAccount || root.subagentKind != "" ||
		(root.threadSource != "" && root.threadSource != "user") ||
		(root.requestKind != "" && root.requestKind != "turn") || passiveInternalRequestAuthorized(c):
		state.diagnostic.Reason = "related_or_internal_request"
		return
	case strings.HasSuffix(c.Request.URL.Path, "/compact") || requestBodyHasCompactionTrigger(body) || inferredSessionHasCompaction(body):
		state.diagnostic.Reason = "compaction_context"
		return
	case strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "":
		state.diagnostic.Reason = "response_continuation"
		return
	}
	if verifiedTransportUser(c.Request.Context()) == "" && requestAPIKeyID(c) <= 0 {
		state.diagnostic.Reason = "authenticated_scope_unavailable"
		return
	}
	anchor := inferredConversationAnchor(body)
	if anchor == "" {
		state.diagnostic.Reason = "conversation_prefix_unavailable"
		return
	}
	device, source := "", "unavailable"
	if verified && policy.MetaVerified {
		device = strings.TrimSpace(policy.Meta.InstallationID)
		if device != "" {
			source = "signed_newapi"
		}
	}
	if device == "" {
		device = resolveDownstreamInstallationID(c.Request.Header, body)
		if device != "" {
			source = "client_installation"
		}
	}
	if device == "" {
		for _, name := range []string{"X-Installation-Id", "X-Device-Id", "Oai-Device-Id"} {
			if value := strings.TrimSpace(c.Request.Header.Get(name)); value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00") {
				device, source = value, "client_device"
				break
			}
		}
	}
	ua := strings.Join(strings.Fields(c.Request.UserAgent()), " ")
	if len(ua) > 2048 {
		ua = ""
	}
	// A generic relay UA is only a partition hint; it is never a device ID.
	seed, _ := json.Marshal([]string{"bps-inferred-conversation-v1", responseCacheOwnerForRequest(c, requestAPIKeyID(c)), device, ua, anchor})
	state.seed = DeriveStableSessionUUIDv7(string(seed))
	state.diagnostic = inferredBPSSessionDiagnostic{Result: "derived", Source: "user_device_conversation_prefix", DeviceSource: source, UAIncluded: ua != "", Heuristic: true}
	if device == "" {
		state.diagnostic.Source = "user_conversation_prefix"
	}
}

func inferredSessionHasCompaction(body []byte) bool {
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if gjsonResultIsCompactionHistory(item) {
			return true
		}
	}
	return false
}

// Preserve the opening instruction prefix and first user message across turns.
// Model, tool catalog and later messages do not change a conversation's hint.
// Identical openings cannot distinguish independent chats without client IDs.
func inferredConversationAnchor(body []byte) string {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return ""
	}
	hash := sha256.New()
	write := func(tag string, value gjson.Result) bool {
		if !value.Exists() || value.Type == gjson.Null || value.Type == gjson.String && strings.TrimSpace(value.String()) == "" {
			return false
		}
		if value.IsArray() && len(value.Array()) == 0 {
			return false
		}
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(value.Raw)) != nil {
			return false
		}
		hash.Write([]byte(tag))
		hash.Write([]byte{0})
		hash.Write(compact.Bytes())
		hash.Write([]byte{0})
		return true
	}
	write("instructions", root.Get("instructions"))
	items := root.Get("input")
	if !items.Exists() {
		items = root.Get("messages")
	}
	if items.Type == gjson.String {
		if !write("user", items) {
			return ""
		}
		return hex.EncodeToString(hash.Sum(nil))
	}
	if !items.IsArray() {
		return ""
	}
	for _, item := range items.Array() {
		switch item.Get("role").String() {
		case "system", "developer":
			write(item.Get("role").String(), item.Get("content"))
		case "user":
			if !write("user", item.Get("content")) {
				return ""
			}
			return hex.EncodeToString(hash.Sum(nil))
		case "assistant":
			return "" // Truncated history is not a reliable opening.
		default:
			if kind := item.Get("type").String(); kind != "additional_tools" {
				return ""
			}
		}
	}
	return ""
}

func inferredBPSCacheSeed(ctx context.Context, account *auth.Account, original string, compact bool) (string, *inferredBPSSessionDiagnostic) {
	if ctx == nil || account == nil || account.EffectiveCodexFingerprintMode() != auth.CodexFingerprintModeSession {
		return original, nil
	}
	state, _ := ctx.Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	if state == nil {
		return original, nil
	}
	diagnostic := state.diagnostic
	if compact {
		diagnostic.Result, diagnostic.Reason = "skipped", "compaction_request"
		return original, &diagnostic
	}
	if state.seed == "" {
		return original, &diagnostic
	}
	diagnostic.Result = "applied"
	return ScopeCodexPromptCacheKey(ctx, state.seed), &diagnostic
}
