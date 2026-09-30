package proxy

// Support code for the BPS transport plugin ported from fj-server. The pure
// helpers are verbatim ports. fj's session-identity, outbound-epoch, failover
// and synthetic turn-state machinery (the dropped A/B/C groups) is replaced by
// the explicit stubs and light equivalents below; each says what it stands in
// for so the BPS files keep their original shape.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexIdentityDigest must stay byte-identical to fj-server: persisted BPS
// identity keys (codex_identity_uuid7_values, bps_*) are derived from it.
func codexIdentityDigest(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func codexAccountIdentityError(message string) *Error {
	return &Error{Code: "codex_session_identity_unavailable", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

const defaultCodexReasoningEffort = "low"

func validSessionGraphUUID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

// JSON's last value wins in both collection and emission. Replacing the whole
// item prevents a second duplicate metadata/turn_id key surviving an sjson edit.
func historyItemMetadata(item gjson.Result) (map[string]json.RawMessage, map[string]json.RawMessage) {
	var object, metadata map[string]json.RawMessage
	_ = json.Unmarshal([]byte(item.Raw), &object)
	_ = json.Unmarshal(object["internal_chat_message_metadata_passthrough"], &metadata)
	return metadata, object
}

func safeDiagnosticToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > 128 || strings.HasPrefix(value, "eyJ") || strings.HasPrefix(value, "sk-") {
		return "hash:" + hashRiskIdentity(value)
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("_-.:", character)) {
			return "hash:" + hashRiskIdentity(value)
		}
	}
	return value
}

// Only protocol shape and identifier hashes are captured. Never collect tool
// descriptions, schemas, arguments, outputs or conversation text here.
type toolProtocolItem struct {
	Path            string `json:"path"`
	Kind            string `json:"kind"`
	NameState       string `json:"name_state"`
	NameHash        string `json:"name_hash,omitempty"`
	NestedNameState string `json:"nested_name_state,omitempty"`
	NestedNameHash  string `json:"nested_name_hash,omitempty"`
	NamespaceState  string `json:"namespace_state"`
	NamespaceHash   string `json:"namespace_hash,omitempty"`
	Issue           string `json:"issue,omitempty"`
}

func toolIdentifierState(value gjson.Result) (string, string) {
	if !value.Exists() {
		return "absent", ""
	}
	if value.Type != gjson.String {
		return "invalid_type", ""
	}
	if strings.TrimSpace(value.String()) == "" {
		return "empty", ""
	}
	digest := sha256.Sum256([]byte(value.String()))
	return "present", hex.EncodeToString(digest[:12])
}

func toolProtocolShape(item gjson.Result, path, kind string) toolProtocolItem {
	shape := toolProtocolItem{Path: path, Kind: kind}
	shape.NameState, shape.NameHash = toolIdentifierState(item.Get("name"))
	shape.NamespaceState, shape.NamespaceHash = toolIdentifierState(item.Get("namespace"))
	for _, field := range []string{"function", "custom"} {
		if item.Get(field).IsObject() {
			shape.NestedNameState, shape.NestedNameHash = toolIdentifierState(item.Get(field + ".name"))
			break
		}
	}
	return shape
}

// Normalize protocol field names only, never values.
func privacyField(key string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(key))
}

func responseBusinessField(key string) bool {
	switch privacyField(key) {
	case "input", "output", "content", "arguments", "tools", "toolchoice", "parameters", "schema", "text", "delta", "instructions", "audio", "image", "refusal":
		return true
	}
	return false
}

// ResponseOpaquePayloadField reports model/tool payload fields at a protocol
// position; the BPS response projection never rewrites them.
func ResponseOpaquePayloadField(kind, key string) bool {
	field := privacyField(key)
	if responseBusinessField(key) && field != "output" {
		return true
	}
	switch kind {
	case "program":
		return field == "code" || field == "fingerprint"
	case "program_output":
		return field == "result"
	case "image_generation_call":
		return field == "result" || field == "revisedprompt"
	case "mcp_approval_response":
		return field == "reason"
	case "reasoning":
		return field == "summary"
	case "computer_call", "web_search_call", "local_shell_call", "shell_call", "apply_patch_call":
		return field == "action" || field == "operation"
	case "code_interpreter_call":
		return field == "code" || field == "outputs"
	case "file_search_call":
		return field == "results" || field == "queries"
	case "function_call_output", "custom_tool_call_output", "tool_call_output", "tool_search_call_output", "computer_call_output", "local_shell_call_output", "shell_call_output", "apply_patch_call_output", "mcp_tool_call_output", "mcp_call":
		return field == "output"
	case "response.code_interpreter_call_code.done":
		return field == "code"
	case "response.shell_call_command.added", "response.shell_call_command.done":
		return field == "command"
	}
	return false
}

// preserveUpstreamSource is fj's signed NewAPI-admin exemption from source
// name rewriting. production-main's NewAPI policy meta has no such flag, so
// source text is always rewritten.
func preserveUpstreamSource(context.Context) bool { return false }

var (
	diagnosticURL   = regexp.MustCompile(`(?i)(?:https?|wss?|socks5h?)://[^\s<>"']+`)
	diagnosticHost  = regexp.MustCompile(`(?i)\b(?:[a-z0-9_-]+\.)+[a-z][a-z0-9-]{1,62}\b`)
	diagnosticIP    = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b|\[[0-9a-fA-F:]+\]`)
	diagnosticToken = regexp.MustCompile(`\b(?:eyJ[A-Za-z0-9_.-]+|[A-Za-z0-9_+/=-]{80,})`)
)

// upstreamErrorSafeMessage bounds and masks an upstream error text before it
// reaches logs or clients (URLs, hosts, IPs and bearer-like tokens removed).
func upstreamErrorSafeMessage(_ *gin.Context, value string) string {
	value = security.MaskSensitiveData(value)
	value = diagnosticURL.ReplaceAllString(value, "[upstream URL]")
	value = diagnosticHost.ReplaceAllStringFunc(value, func(host string) string {
		if strings.EqualFold(host, "config.toml") {
			return host
		}
		return "[host]"
	})
	value = diagnosticIP.ReplaceAllString(value, "[IP]")
	value = diagnosticToken.ReplaceAllString(value, "[REDACTED]")
	return security.SafeTruncate(strings.Join(strings.Fields(value), " "), 1000)
}

func isUpstreamPromptSafetyRefusal(payload []byte) bool {
	if isExplicitUpstreamCyberPolicy(payload) {
		return false
	}
	body := gjson.ParseBytes(responseFailedErrorBody(payload))
	for _, value := range []gjson.Result{body.Get("error"), body} {
		if !value.IsObject() || !strings.EqualFold(strings.TrimSpace(value.Get("code").String()), "invalid_prompt") {
			continue
		}
		message := strings.ToLower(strings.Join(strings.Fields(value.Get("message").String()), " "))
		message = strings.TrimPrefix(message, "invalid prompt: ")
		if strings.HasPrefix(message, "your prompt was flagged as potentially violating our usage policy") {
			return true
		}
	}
	return false
}

// isHardStopUpstreamPolicyError reports cyber-policy and prompt-safety
// refusals, which must never be retried by re-uploading or rotating accounts.
func isHardStopUpstreamPolicyError(err error) bool {
	for depth := 0; err != nil && depth < 16; depth++ {
		if upstream, ok := err.(continuousRetryHTTPError); ok {
			body := upstream.UpstreamErrorBody()
			if isExplicitUpstreamCyberPolicy(body) || isUpstreamPromptSafetyRefusal(body) {
				return true
			}
		}
		err = errors.Unwrap(err)
	}
	return false
}

func rateLimitRequestError(err error) bool {
	var upstream *Error
	return errors.As(err, &upstream) && upstream.HTTPStatus == http.StatusTooManyRequests
}

func currentFirstTokenMode() string { return CurrentRuntimeSettings().FirstTokenMode }

// Outbound epochs (fj session failover, group B) do not exist here: every
// caller sees "no epoch", so turn isolation falls back to the binding revision
// of the BPS task affinity and account identity alone.
type bpsOutboundEpoch struct{}

func outboundEpochFromContext(context.Context) *bpsOutboundEpoch { return nil }

func (*bpsOutboundEpoch) identityKey() string { return "" }

// ScopeCodexPromptCacheKey was fj's per-epoch cache scoping; without epochs the
// key is already scoped by caller and account.
func ScopeCodexPromptCacheKey(_ context.Context, cacheKey string) string { return cacheKey }

// passiveInternalRequestAuthorized was fj's signed passive-request grant; no
// such grant exists here.
func passiveInternalRequestAuthorized(*gin.Context) bool { return false }

type bpsCallerContextKey struct{}

// bpsCaller is the "caller owner" of a BPS request: the verified NewAPI user
// when the request carries a verified policy, otherwise empty (callers then
// fall back to an API-key digest).
type bpsCaller struct {
	user string
}

func withBPSCaller(c *gin.Context, ctx context.Context) context.Context {
	caller := bpsCaller{}
	if c != nil {
		if value, ok := c.Get(newAPIPolicyMetaContextKey); ok {
			if policy, ok := value.(verifiedNewAPIPolicyContext); ok && policy.MetaVerified && strings.TrimSpace(policy.Identity.UserID) != "" {
				encoded, _ := json.Marshal([]string{newAPIRuntimeScope(policy.APIKeyID, policy.Platform), policy.Identity.UserID})
				caller.user = hashRiskIdentity(string(encoded))
			}
		}
	}
	return context.WithValue(ctx, bpsCallerContextKey{}, caller)
}

func verifiedTransportUser(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	caller, _ := ctx.Value(bpsCallerContextKey{}).(bpsCaller)
	return caller.user
}

func responseCacheOwnerForRequest(c *gin.Context, apiKeyID int64) string {
	if user := verifiedTransportUser(c.Request.Context()); user != "" {
		return responseCacheOwner(apiKeyID) + ":user:" + user
	}
	if apiKeyID > 0 {
		return responseCacheOwner(apiKeyID)
	}
	if credential := downstreamAuthorizationHeader(c.Request); credential != "" {
		return "credential:" + hashRiskIdentity(credential)
	}
	const key = "bps-anonymous-owner"
	if owner := c.GetString(key); owner != "" {
		return owner
	}
	owner := "anonymous:" + NewUpstreamSessionUUID()
	c.Set(key, owner)
	return owner
}

// resolveDownstreamInstallationID reads the client device marker from headers
// or turn metadata. It narrows inferred-session scope only.
func resolveDownstreamInstallationID(headers http.Header, body []byte) string {
	valid := func(value string) bool {
		return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
	}
	if value := strings.TrimSpace(headers.Get(codexInstallationIDHeader)); valid(value) {
		return value
	}
	metadata := CodexRequestMetadataHeaders(headers, body).Get(codexTurnMetadataHeader)
	if value := strings.TrimSpace(gjson.Get(metadata, "installation_id").String()); valid(value) {
		return value
	}
	if value := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.installation_id").String()); valid(value) {
		return value
	}
	return ""
}

// NormalizeCodexRequestMetadata projects the canonical x-codex-turn-metadata
// values onto the flat client_metadata keys (fj-server, unchanged).
func NormalizeCodexRequestMetadata(body []byte) []byte {
	metadata := gjson.GetBytes(body, "client_metadata")
	canonical := metadata.Get("x-codex-turn-metadata")
	if canonical.Type == gjson.String {
		if !gjson.Valid(canonical.String()) {
			return body
		}
		canonical = gjson.Parse(canonical.String())
	}
	if !metadata.IsObject() || !canonical.IsObject() {
		return body
	}
	for _, projection := range [][2]string{
		{"session_id", "session_id"}, {"thread_id", "thread_id"},
		{"window_id", "window_id"}, {"window_id", "x-codex-window-id"}, {"window_number", "window_number"},
		{"installation_id", "installation_id"}, {"installation_id", "x-codex-installation-id"},
		{"context_window_id", "context_window_id"}, {"context_window_id", "x-codex-context-window-id"},
		{"turn_id", "turn_id"}, {"root_turn_id", "root_turn_id"}, {"parent_turn_id", "parent_turn_id"},
		{"guardian_classifier_source_thread_id", "guardian_classifier_source_thread_id"},
		{"parent_thread_id", "parent_thread_id"}, {"parent_thread_id", "x-codex-parent-thread-id"},
		{"forked_from_thread_id", "forked_from_thread_id"}, {"forked_from_thread_id", "x-codex-forked-from-thread-id"},
		{"subagent_kind", "subagent_kind"},
		{"thread_source", "thread_source"}, {"request_kind", "request_kind"},
	} {
		value, flat := canonical.Get(projection[0]), metadata.Get(projection[1])
		if !value.Exists() || !flat.Exists() {
			continue
		}
		path := "client_metadata." + projection[1]
		var updated []byte
		var err error
		if value.Type == gjson.Null || value.Type == gjson.String && strings.TrimSpace(value.String()) == "" {
			updated, err = sjson.DeleteBytes(body, path)
		} else if flat.Raw != value.Raw {
			updated, err = sjson.SetRawBytes(body, path, []byte(value.Raw))
		} else {
			continue
		}
		if err == nil {
			body = updated
		}
	}
	return body
}

// CodexRequestMetadataHeaders resolves the effective Codex identity headers of
// a request: a frame's embedded x-codex-turn-metadata wins over the HTTP
// headers, and the flat client_metadata keys fill the rest. This is the small
// parser BPS needs; fj's passive/guardian marker projection is not carried.
func CodexRequestMetadataHeaders(headers http.Header, body []byte) http.Header {
	body = NormalizeCodexRequestMetadata(body)
	resolved := headers.Clone()
	if resolved == nil {
		resolved = make(http.Header)
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return resolved
	}
	embedded := metadata.Get("x-codex-turn-metadata")
	canonical := embedded
	if embedded.Type == gjson.String {
		canonical = gjson.Parse(embedded.String())
	}
	frameSnapshot := embedded.Exists()
	if frameSnapshot {
		resolved.Del(codexTurnMetadataHeader)
		if canonical.IsObject() {
			resolved.Set(codexTurnMetadataHeader, canonical.Raw)
		}
	}
	for _, projection := range []struct{ header, flat, field string }{
		{codexSessionIDHeader, "session_id", "session_id"},
		{codexThreadIDHeader, "thread_id", "thread_id"},
		{codexWindowIDHeader, "x-codex-window-id", "window_id"},
		{codexParentThreadIDHeader, "x-codex-parent-thread-id", "parent_thread_id"},
	} {
		value := metadata.Get(projection.flat)
		if canonical.IsObject() {
			if current := canonical.Get(projection.field); current.Exists() {
				value = current
			}
		}
		if !frameSnapshot && !value.Exists() {
			continue
		}
		resolved.Del(projection.header)
		if projection.header == codexSessionIDHeader {
			resolved.Del(codexLegacySessionIDHeader)
		}
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			resolved.Set(projection.header, strings.TrimSpace(value.String()))
		}
	}
	// Passive markers: the canonical subagent kind, else the flat
	// client_metadata marker (fj codexPassiveMarkers, subagent part only).
	kind := strings.TrimSpace(canonical.Get("subagent_kind").String())
	if kind == "" {
		kind = strings.TrimSpace(metadata.Get("x-openai-subagent").String())
	}
	if kind != "" && resolved.Get("X-OpenAI-Subagent") == "" {
		if kind == "thread_spawn" {
			kind = "collab_spawn"
		}
		resolved.Set("X-OpenAI-Subagent", kind)
	}
	return resolved
}

// requestRootSessionIdentity is a light request classifier replacing fj's
// root-session graph resolver (group A). It reports whether the request names
// a session, whether it is a related/background request and the Codex
// request/thread/subagent kinds. It does not detect cross-carrier conflicts.
type requestRootSessionIdentity struct {
	stable       bool
	conflict     bool
	related      bool
	threadSource string
	requestKind  string
	subagentKind string
}

func resolveRequestRootSessionIdentity(headers http.Header, body []byte) requestRootSessionIdentity {
	resolved := CodexRequestMetadataHeaders(headers, body)
	meta := gjson.Parse(resolved.Get(codexTurnMetadataHeader))
	session := strings.TrimSpace(meta.Get("session_id").String())
	if session == "" {
		session = strings.TrimSpace(resolved.Get(codexSessionIDHeader))
	}
	if session == "" {
		session = strings.TrimSpace(resolved.Get(codexLegacySessionIDHeader))
	}
	thread := strings.TrimSpace(meta.Get("thread_id").String())
	if thread == "" {
		thread = strings.TrimSpace(resolved.Get(codexThreadIDHeader))
	}
	parent := strings.TrimSpace(meta.Get("parent_thread_id").String())
	if parent == "" {
		parent = strings.TrimSpace(resolved.Get(codexParentThreadIDHeader))
	}
	root := requestRootSessionIdentity{
		stable:       session != "",
		threadSource: strings.ToLower(strings.TrimSpace(meta.Get("thread_source").String())),
		requestKind:  strings.ToLower(strings.TrimSpace(meta.Get("request_kind").String())),
		subagentKind: strings.ToLower(strings.TrimSpace(meta.Get("subagent_kind").String())),
	}
	forked := strings.TrimSpace(meta.Get("forked_from_thread_id").String())
	root.related = parent != "" || forked != "" || root.subagentKind != "" || resolved.Get("X-OpenAI-Subagent") != "" ||
		(thread != "" && session != "" && !strings.EqualFold(thread, session))
	return root
}

func diagnosticMetadataObject(raw gjson.Result) gjson.Result {
	if raw.Type == gjson.String && gjson.Valid(raw.String()) {
		return gjson.Parse(raw.String())
	}
	return raw
}

// bpsError builds a request error in the plugin's error shape.
func bpsError(status int, code, format string, args ...any) *Error {
	kind := ErrorTypeInvalidRequest
	if status >= 500 {
		kind = ErrorTypeServerError
	}
	return &Error{Code: code, Type: kind, HTTPStatus: status, Message: fmt.Sprintf(format, args...)}
}
