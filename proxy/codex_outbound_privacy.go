package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Outbound metadata is a protocol, not an arbitrary user-controlled JSON bag.
// Resolve each field once, then project that snapshot into all wire carriers.
// Never apply this policy to input, tool arguments/results, or JSON schemas.
var codexOutboundMetadataFields = map[string]string{
	"session_id": "Session-Id", "thread_id": "Thread-Id",
	"parent_thread_id": "X-Codex-Parent-Thread-Id", "forked_from_thread_id": "X-Codex-Forked-From-Thread-Id",
	"context_window_id": "X-Codex-Context-Window-Id", "window_id": "X-Codex-Window-Id",
	"installation_id": "X-Codex-Installation-Id", "client_request_id": "X-Client-Request-Id",
	"turn_id": "", "root_turn_id": "", "parent_turn_id": "", "guardian_classifier_source_thread_id": "",
	"window_number": "", "turn_started_at_unix_ms": "", "analytics_enabled": "",
	"thread_source": "", "request_kind": "", "subagent_kind": "",
	"approval_policy": "", "sandbox": "", "x-codex-turn-state": "X-Codex-Turn-State",
	"agent_name": "", "forked_from_ordinal_exclusive": "", "turn_trigger": "", "sandbox_mode": "",
	"auto_review_enabled": "", "node_repl_auto_review_required": "", "node_repl_disabled": "",
	"history_ingest_requested": "", "model": "", "reasoning_effort": "", "workspace_kind": "",
}

var codexOutboundFlatFields = map[string]string{
	"session_id": "session_id", "thread_id": "thread_id",
	"parent_thread_id": "x-codex-parent-thread-id", "forked_from_thread_id": "x-codex-forked-from-thread-id",
	"context_window_id": "x-codex-context-window-id", "window_id": "x-codex-window-id",
	"installation_id": "x-codex-installation-id", "client_request_id": "x-client-request-id",
	"x-codex-turn-state": "x-codex-turn-state",
}

var codexOutboundRequestFields = map[string]bool{
	"model": true, "input": true, "instructions": true, "tools": true,
	"additional_tools": true, "tool_choice": true, "parallel_tool_calls": true,
	"reasoning": true, "text": true, "include": true, "store": true, "stream": true,
	"service_tier": true, "prompt_cache_key": true, "prompt_cache_retention": true,
	"previous_response_id": true, "type": true, "background": true,
	"max_output_tokens": true, "max_tool_calls": true, "truncation": true,
	"temperature": true, "top_p": true, "top_logprobs": true, "stream_options": true,
	"context_management": true, "prompt": true, "conversation": true,
	"access_programs": true, "generate": true, "stream_id": true,
	"prompt_cache_options": true, "moderation": true, "metadata": true,
	"safety_identifier": true, "user": true,
}

func outboundMetadataField(key string) string {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch key {
	case "x_codex_turn_metadata":
		return "x-codex-turn-metadata"
	case "x_codex_turn_state":
		return "x-codex-turn-state"
	case "x_client_request_id":
		return "client_request_id"
	case "x_openai_subagent":
		return "subagent_header"
	case "x_openai_memgen_request":
		return "memgen_request"
	}
	return strings.TrimPrefix(key, "x_codex_")
}

func outboundMetadataObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		return object
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		_ = json.Unmarshal([]byte(encoded), &object)
	}
	return object
}

// Reject ambiguous control keys before routing or protocol translation can
// choose a different duplicate than the outbound encoder. Business input is
// deliberately not traversed.
func validateCodexMetadataDuplicates(raw []byte, control bool, depth int) error {
	if depth > 32 {
		return codexAccountIdentityError("身份元数据嵌套过深，请检查客户端请求。")
	}
	value := gjson.ParseBytes(raw)
	if value.Type == gjson.String && control && gjson.Valid(value.String()) {
		return validateCodexMetadataDuplicates([]byte(value.String()), true, depth+1)
	}
	if !value.IsObject() {
		return nil
	}
	seen := make(map[string]gjson.Result)
	var failure error
	value.ForEach(func(key, child gjson.Result) bool {
		name := key.String()
		field := outboundMetadataField(name)
		container := field == "client_metadata" || field == "x-codex-turn-metadata"
		_, identity := codexOutboundMetadataFields[field]
		if container || control && (identity || field == "subagent_header" || field == "memgen_request") {
			if previous, exists := seen[name]; exists && (container || previous.Raw != child.Raw && previous.String() != child.String()) {
				failure = codexAccountIdentityError("身份元数据包含冲突的重复字段，请检查客户端请求。")
				return false
			}
			seen[name] = child
		}
		if container {
			failure = validateCodexMetadataDuplicates([]byte(child.Raw), true, depth+1)
		}
		return failure == nil
	})
	return failure
}

// Canonical spelling wins over compatibility aliases; source order is handled
// by the caller. Maps use encoding/json's last-key semantics consistently.
func outboundMetadataAliases(object map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := outboundMetadataField(key)
		if _, exists := result[name]; !exists || key == name {
			result[name] = object[key]
		}
	}
	return result
}

func validOutboundMetadataScalar(field string, raw json.RawMessage) bool {
	v := gjson.ParseBytes(raw)
	switch field {
	case "window_number", "turn_started_at_unix_ms", "forked_from_ordinal_exclusive":
		return v.Type == gjson.Number && v.Int() >= 0
	case "analytics_enabled", "auto_review_enabled", "node_repl_auto_review_required", "node_repl_disabled", "history_ingest_requested":
		return v.Type == gjson.True || v.Type == gjson.False
	default:
		return v.Type == gjson.String && strings.TrimSpace(v.String()) != "" && (field == "x-codex-turn-state" || len(v.String()) <= 1024)
	}
}

func sanitizeOutboundWorkspaces(raw json.RawMessage, account *auth.Account) json.RawMessage {
	workspaces := outboundMetadataObject(raw)
	if workspaces == nil || account == nil || account.ID() <= 0 {
		return nil
	}
	clean := make(map[string]map[string]json.RawMessage)
	for path, value := range workspaces {
		entry := outboundMetadataObject(value)
		if entry == nil {
			continue
		}
		fields := make(map[string]json.RawMessage)
		if commit := entry["latest_git_commit_hash"]; gjson.ParseBytes(commit).Type == gjson.String {
			fields["latest_git_commit_hash"] = commit
		}
		if changed := entry["has_changes"]; gjson.ParseBytes(changed).Type == gjson.True || gjson.ParseBytes(changed).Type == gjson.False {
			fields["has_changes"] = changed
		}
		clean[path] = fields
	}
	encoded, _ := json.Marshal(map[string]any{"workspaces": clean})
	// Reuse the existing account-scoped path/commit transformation.
	rewritten, _ := scrubCodexWorkspaces(string(encoded), account.ID())
	return json.RawMessage(gjson.Get(rewritten, "workspaces").Raw)
}

// PrepareCodexOutboundMetadata runs after routing/owner resolution and before
// identity claiming. It only changes a detached outbound copy.
func PrepareCodexOutboundMetadata(account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header) {
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || top == nil {
		return body, headers
	}
	originalFlat := outboundMetadataObject(top["client_metadata"])
	flat := outboundMetadataAliases(originalFlat)
	embedded, hasSnapshot := flat["x-codex-turn-metadata"]
	subagent, memgen := codexPassiveMarkers(headers, gjson.ParseBytes(top["client_metadata"]), diagnosticMetadataObject(gjson.ParseBytes(embedded)), hasSnapshot)
	canonical := outboundMetadataAliases(outboundMetadataObject(embedded))
	fromHeader := outboundMetadataAliases(outboundMetadataObject(json.RawMessage(headers.Get(codexTurnMetadataHeader))))
	values := make(map[string]json.RawMessage)
	for field, header := range codexOutboundMetadataFields {
		// A current frame snapshot suppresses stale handshake fields. Only the
		// compatibility projections present on that frame may fill omissions.
		value, exists := canonical[field]
		if !exists {
			value, exists = flat[field]
		}
		if !exists && !hasSnapshot {
			value, exists = fromHeader[field]
		}
		if !exists && !hasSnapshot && header != "" {
			v := headers.Get(header)
			if field == "session_id" && v == "" {
				v = headers.Get(codexLegacySessionIDHeader)
			}
			if v != "" {
				value, _ = json.Marshal(v)
			}
		}
		value = normalizeOutboundMetadataScalar(field, value)
		if validOutboundMetadataScalar(field, value) {
			values[field] = value
		}
	}
	// Hardware/device identity comes from the selected account, never the caller.
	if account != nil && account.ID() > 0 && (values["installation_id"] != nil) {
		values["installation_id"], _ = json.Marshal(resolveConvergedInstallationID(account, account.ID()))
	}
	if values["client_request_id"] == nil && values["thread_id"] != nil {
		values["client_request_id"] = values["thread_id"]
	}
	if raw := canonical["compaction"]; raw != nil {
		labels := make(map[string]json.RawMessage)
		object := outboundMetadataObject(raw)
		for _, field := range []string{"implementation", "trigger", "reason", "phase", "strategy"} {
			if v := gjson.ParseBytes(object[field]); v.Type == gjson.String && sessionErrorLabel(v.String()) {
				labels[field] = object[field]
			}
		}
		if len(labels) > 0 {
			values["compaction"], _ = json.Marshal(labels)
		}
	}
	if raw := canonical["workspaces"]; raw != nil {
		if clean := sanitizeOutboundWorkspaces(raw, account); clean != nil {
			values["workspaces"] = clean
		}
	}
	if raw := canonical["tool_namespaces_info"]; raw != nil {
		if clean := sanitizeCodexToolInventory(raw); clean != nil {
			values["tool_namespaces_info"] = clean
		}
	}
	// Drop non-protocol top-level identity envelopes rather than recursively
	// interpreting arbitrary objects as a second, conflicting session identity.
	for key := range top {
		name := outboundMetadataField(key)
		_, identity := codexOutboundMetadataFields[name]
		switch strings.ToLower(strings.ReplaceAll(key, "-", "_")) {
		case "extra_body", "headers", "account_id", "request_id", "device_id", "machine_id", "user_id", "organization_id", "email", "ip", "hostname", "project_id", "projectid", "workspace_id", "x_oai_attestation", "client_metadata", "x_codex_turn_metadata":
			identity = true
		}
		if identity && key != "model" || !codexOutboundRequestFields[key] {
			delete(top, key)
		}
	}
	cleanFlat := make(map[string]json.RawMessage)
	for key, raw := range originalFlat {
		if codexExtraMetadataField(key, gjson.ParseBytes(raw)) {
			cleanFlat[key] = raw
		}
	}
	for _, field := range codexFlatControlFields {
		if raw := flat[outboundMetadataField(field)]; raw != nil {
			cleanFlat[field] = raw
		}
	}
	markerMetadata := diagnosticMetadataObject(gjson.ParseBytes(embedded))
	if !hasSnapshot {
		markerMetadata = gjson.Parse(headers.Get(codexTurnMetadataHeader))
	}
	mode := codexGuardianMode(headers, markerMetadata)
	headers.Del("X-Codex-Guardian")
	if mode != "" {
		headers.Set("X-Codex-Guardian", mode)
	}
	// Official non-basic sessions include ordinary user turns. Preserve an
	// explicit credit request; only the reviewer route suppresses this flag.
	if mode == "reviewer" {
		delete(cleanFlat, "guardian_credits_requested")
	} else if v := gjson.ParseBytes(cleanFlat["guardian_credits_requested"]); v.String() != "true" {
		delete(cleanFlat, "guardian_credits_requested")
	} else {
		cleanFlat["guardian_credits_requested"] = json.RawMessage(`"true"`)
	}
	if gjson.GetBytes(body, "type").String() == "response.create" {
		for _, name := range []string{"traceparent", "tracestate"} {
			key := "ws_request_header_" + name
			if cleanFlat[key] == nil && headers.Get(name) != "" {
				cleanFlat[key], _ = json.Marshal(headers.Get(name))
			}
		}
	}
	for field, value := range map[string]string{"x-openai-subagent": subagent, "x-openai-memgen-request": memgen} {
		if value != "" {
			cleanFlat[field], _ = json.Marshal(value)
		}
	}
	// This is a capability flag consumed by HTTP/WS fallback, not identity.
	if lite := flat["ws_request_header_x_openai_internal_codex_responses_lite"]; strings.EqualFold(gjson.ParseBytes(lite).String(), "true") {
		cleanFlat["ws_request_header_x_openai_internal_codex_responses_lite"] = json.RawMessage(`"true"`)
	}
	for key := range originalFlat {
		field := outboundMetadataField(key)
		if value := values[field]; value != nil && field != "workspaces" && field != "compaction" && field != "tool_namespaces_info" {
			name := field
			if preferred := codexOutboundFlatFields[field]; preferred != "" && key != field {
				name = preferred
			}
			cleanFlat[name] = value
		}
	}
	for field, alias := range codexOutboundFlatFields {
		if value := values[field]; value != nil {
			cleanFlat[alias] = value
		}
	}
	// Preserve the client's canonical object/string shape without duplicate carriers.
	extra := outboundMetadataObject(embedded)
	if !hasSnapshot {
		extra = outboundMetadataObject(json.RawMessage(headers.Get(codexTurnMetadataHeader)))
	}
	if len(values) > 0 || len(extra) > 0 {
		wireValues := make(map[string]json.RawMessage, len(values))
		for field, value := range values {
			if codexTurnMetadataOmitsField(markerMetadata, field) {
				continue
			}
			wireValues[field] = value
		}
		for key, raw := range extra {
			if codexExtraMetadataField(key, gjson.ParseBytes(raw)) {
				wireValues[key] = raw
			}
		}
		// The official flat/header client-request carrier need not introduce a
		// new field in a client's otherwise unchanged turn-metadata schema.
		if canonical["client_request_id"] == nil && fromHeader["client_request_id"] == nil {
			delete(wireValues, "client_request_id")
		}
		if canonical["x-codex-turn-state"] == nil && fromHeader["x-codex-turn-state"] == nil {
			delete(wireValues, "x-codex-turn-state")
		}
		raw, _ := json.Marshal(wireValues)
		raw = []byte(omitCodexTurnMetadataIdentity(gjson.ParseBytes(raw)).Raw)
		if gjson.ParseBytes(embedded).IsObject() {
			cleanFlat["x-codex-turn-metadata"] = raw
		} else {
			cleanFlat["x-codex-turn-metadata"], _ = json.Marshal(string(raw))
		}
		headers.Set(codexTurnMetadataHeader, string(raw))
	} else {
		headers.Del(codexTurnMetadataHeader)
	}
	if len(cleanFlat) > 0 {
		top["client_metadata"], _ = json.Marshal(cleanFlat)
	}
	for field, name := range codexOutboundMetadataFields {
		if name == "" {
			continue
		}
		deleteHeaderCaseInsensitive(headers, name)
		if value := values[field]; value != nil {
			headers.Set(name, gjson.ParseBytes(value).String())
		}
	}
	setCodexPassiveHeaders(headers, subagent, memgen)
	headers.Del(codexLegacySessionIDHeader)
	deleteHeaderCaseInsensitive(headers, "X-Oai-Attestation")
	out, err := json.Marshal(top)
	if err != nil {
		return body, headers
	}
	return out, headers
}

// FinalizeCodexOutboundMetadata projects already-mapped values only. It must
// never hash values again: retries and WS frames reuse the same mapping.
func FinalizeCodexOutboundMetadata(body []byte, headers http.Header, contexts ...context.Context) ([]byte, http.Header) {
	body, headers = finalizeCodexOutboundMetadata(body, headers)
	if len(contexts) > 0 {
		body, headers = finalizeProjectControlMetadata(contexts[0], body, headers)
	}
	return body, headers
}

func finalizeCodexOutboundMetadata(body []byte, headers http.Header) ([]byte, http.Header) {
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	canonical := diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata"))
	mode := codexGuardianMode(headers, canonical)
	deleteHeaderCaseInsensitive(headers, "X-Codex-Guardian")
	if mode != "" {
		headers.Set("X-Codex-Guardian", mode)
	}
	if !canonical.IsObject() {
		return encodeCodexClientMetadata(body), headers
	}
	// Descriptive execution metadata must describe the actual request after model,
	// effort and transport rules have run, not a stale client snapshot.
	for _, field := range []struct{ metadata, request string }{{"model", "model"}, {"reasoning_effort", "reasoning.effort"}} {
		if canonical.Get(field.metadata).Exists() {
			raw := canonical.Raw
			if actual := gjson.GetBytes(body, field.request); actual.Type == gjson.String {
				raw, _ = sjson.Set(raw, field.metadata, actual.String())
			} else {
				raw, _ = sjson.Delete(raw, field.metadata)
				body, _ = sjson.DeleteBytes(body, "client_metadata."+field.metadata)
			}
			canonical = gjson.Parse(raw)
		}
	}
	canonical = omitCodexTurnMetadataIdentity(canonical)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", canonical.Raw)
	for field, name := range codexOutboundMetadataFields {
		value := canonical.Get(field)
		if codexTurnMetadataOmitsField(canonical, field) {
			value = metadata.Get(field)
			if alias := codexOutboundFlatFields[field]; !value.Exists() && alias != "" {
				value = metadata.Get(alias)
			}
		}
		if field == "client_request_id" {
			if flat := metadata.Get("x-client-request-id"); flat.Exists() {
				value = flat
			}
		}
		if field == "x-codex-turn-state" {
			if flat := metadata.Get(field); flat.Exists() {
				value = flat
			}
		}
		if !value.Exists() {
			if name != "" {
				deleteHeaderCaseInsensitive(headers, name)
			}
			continue
		}
		for _, flat := range []string{field, codexOutboundFlatFields[field]} {
			if flat != "" && metadata.Get(flat).Exists() {
				body, _ = sjson.SetRawBytes(body, "client_metadata."+flat, []byte(value.Raw))
			}
		}
		if name != "" {
			deleteHeaderCaseInsensitive(headers, name)
			if value.Type == gjson.String {
				headers.Set(name, value.String())
			}
		}
	}
	body = NormalizeCodexRequestMetadata(body)
	subagent, memgen := codexPassiveMarkers(headers, gjson.GetBytes(body, "client_metadata"), canonical, true)
	setCodexPassiveHeaders(headers, subagent, memgen)
	body = encodeCodexClientMetadata(body)
	ApplyCodexAnalyticsHeader(headers, body)
	headers.Del(codexLegacySessionIDHeader)
	headers.Del(codexConversationIDHeader)
	headers.Del("Conversation-Id")
	return body, headers
}

func finalizeRelayOutboundHeaders(body []byte, headers http.Header, contexts ...context.Context) http.Header {
	original := headers.Clone()
	_, headers = FinalizeCodexOutboundMetadata(body, headers, contexts...)
	// Preserve provider capability choices: align carriers that are sent,
	// without turning the relay's header passthrough feature on implicitly.
	for _, name := range codexOutboundMetadataFields {
		if name != "" && original.Get(name) == "" {
			headers.Del(name)
		}
	}
	for _, name := range []string{"X-OpenAI-Subagent", "X-OpenAI-Memgen-Request"} {
		if original.Get(name) == "" {
			headers.Del(name)
		}
	}
	if original.Get(codexTurnMetadataHeader) == "" {
		headers.Del(codexTurnMetadataHeader)
	}
	return headers
}

// Only account/operator credentials may supply attestation. No client token is
// reused, synthesized, or automatically cached as an account credential.
func ApplyCodexAccountAttestation(headers http.Header, account *auth.Account) {
	if headers == nil {
		return
	}
	deleteHeaderCaseInsensitive(headers, "X-Oai-Attestation")
	if value := codexAccountHeader(account, "X-Oai-Attestation"); value != "" {
		headers.Set("X-Oai-Attestation", value)
	}
}

func codexAccountHeader(account *auth.Account, name string) string {
	if account == nil {
		return ""
	}
	configured := account.GetCustomHeaders()
	keys := make([]string, 0, len(configured))
	for key := range configured {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(configured[key])
		}
	}
	return ""
}

func (fingerprint *CodexFingerprint) preparePrivateRequestIdentity(ctx context.Context, account *auth.Account, apiKey string) {
	if fingerprint == nil || account == nil || account.IsRelayStyle() || fingerprint.accountIdentity != nil || fingerprint.privateRequestIdentity != nil {
		return
	}
	original := fingerprint.headers.Get(codexClientRequestIDHeader)
	if original == "" || original == fingerprint.headers.Get(codexThreadIDHeader) || original == fingerprint.headers.Get(codexSessionIDHeader) {
		return
	}
	owner := verifiedTransportUser(ctx)
	if owner == "" {
		owner = apiKey
	}
	if owner == "" && ctx != nil {
		owner, _ = ctx.Value(codexAnonymousIdentityContextKey{}).(string)
	}
	alias := DeriveStableSessionUUIDv7(codexIdentityDigest("private-request-v1", fmt.Sprint(account.ID()), resolveConvergedInstallationID(account, account.ID()), owner, original))
	fingerprint.privateRequestIdentity = &codexAccountIdentity{requestAliases: map[string]string{original: alias}}
	fingerprint.headers = fingerprint.privateRequestIdentity.rewriteHeaders(fingerprint.headers)
}

// ValidateCodexOutboundMetadata checks the final wire representation. Missing
// fields are valid for compact and connection-only WS handshake snapshots;
// two present representations of the same field may never disagree.
func ValidateCodexOutboundMetadata(body []byte, headers http.Header) error {
	flat := gjson.GetBytes(body, "client_metadata")
	bodyMetadata := diagnosticMetadataObject(flat.Get("x-codex-turn-metadata"))
	headerMetadata := gjson.Parse(headers.Get(codexTurnMetadataHeader))
	for field, header := range codexOutboundMetadataFields {
		var expected string
		have := false
		check := func(v gjson.Result) bool {
			if !v.Exists() || v.Type == gjson.Null {
				return true
			}
			value := v.String()
			if !have {
				expected, have = value, true
				return true
			}
			return expected == value
		}
		for _, v := range []gjson.Result{bodyMetadata.Get(field), headerMetadata.Get(field), flat.Get(field)} {
			if !check(v) {
				return codexAccountIdentityError("出站身份字段不一致，已停止发送：" + field)
			}
		}
		if alias := codexOutboundFlatFields[field]; alias != "" && !check(flat.Get(alias)) {
			return codexAccountIdentityError("出站身份字段不一致，已停止发送：" + field)
		}
		if header != "" && headers.Get(header) != "" && have && headers.Get(header) != expected {
			return codexAccountIdentityError("出站身份头与正文不一致，已停止发送：" + field)
		}
	}
	for field, header := range map[string]string{"x-openai-subagent": "X-OpenAI-Subagent", "x-openai-memgen-request": "X-OpenAI-Memgen-Request"} {
		if value := flat.Get(field); value.Exists() && headers.Get(header) != "" && value.String() != headers.Get(header) {
			return codexAccountIdentityError("出站标记头与正文不一致，已停止发送：" + field)
		}
	}
	if kind := bodyMetadata.Get("subagent_kind"); kind.Exists() {
		expected := codexSubagentHeaderValue(kind.String())
		if value := flat.Get("x-openai-subagent"); value.Exists() && value.String() != expected || headers.Get("X-OpenAI-Subagent") != "" && headers.Get("X-OpenAI-Subagent") != expected {
			return codexAccountIdentityError("出站子代理标记与类型不一致，已停止发送。")
		}
	}
	return validateProjectControlMetadata(body, headers)
}

// Relay providers share the metadata policy, but must not use the native
// ChatGPT account/UUID admission rules. Their account-scoped key is persistent
// when a store is available and independent of response-ID restoration.
func prepareRelayOutboundPrivacy(ctx context.Context, account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header, error) {
	if account == nil || account.UpstreamType != auth.UpstreamOpenAIResponses {
		return body, headers, nil
	}
	hadMetadata := gjson.GetBytes(body, "client_metadata").Exists()
	body, headers = PrepareCodexOutboundMetadata(account, body, headers)
	base, credential := account.OpenAIResponsesCredentials()
	owner := verifiedTransportUser(ctx)
	if owner == "" {
		owner = codexIdentityDigest("relay-caller", headers.Get("Authorization"))
	}
	key := codexIdentityDigest("relay-outbound-v1", fmt.Sprint(account.ID()), base, owner)
	secret := []byte(credential)
	if store, ok := ctx.Value(codexIdentityClaimerContextKey{}).(CodexIdentityStore); ok {
		policy, err := store.ResolveCodexIdentityMapping(ctx, key, nil, true)
		if err != nil {
			return nil, nil, codexAccountIdentityError("暂时无法读取出站身份映射，请重试。")
		}
		decoded, err := hex.DecodeString(policy.Secret)
		if err != nil || len(decoded) != 32 {
			return nil, nil, codexAccountIdentityError("出站身份映射密钥不可用，请检查持久化存储。")
		}
		secret = decoded
	}
	derive := func(domain, original string) string {
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(key + "\x00" + domain + "\x00" + original))
		return DeriveStableSessionUUIDv7(hex.EncodeToString(mac.Sum(nil)))
	}
	canonical := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
	if canonical.IsObject() {
		originalRequest := gjson.GetBytes(body, "client_metadata.x-client-request-id").String()
		raw := canonical.Raw
		for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "context_window_id", "guardian_classifier_source_thread_id", "client_request_id", "turn_id", "root_turn_id", "parent_turn_id", "window_id"} {
			v := canonical.Get(field)
			if !v.Exists() && codexTurnMetadataOmitsField(canonical, field) {
				v = gjson.GetBytes(body, "client_metadata."+field)
				if alias := codexOutboundFlatFields[field]; !v.Exists() && alias != "" {
					v = gjson.GetBytes(body, "client_metadata."+alias)
				}
			}
			if v.Type != gjson.String {
				continue
			}
			domain := "identity"
			if field == "turn_id" || field == "root_turn_id" || field == "parent_turn_id" {
				domain = "turn"
			}
			original, suffix := v.String(), ""
			if field == "window_id" {
				if i := strings.LastIndexByte(original, ':'); i > 0 {
					original, suffix = original[:i], original[i:]
				}
			}
			mapped := derive(domain, original) + suffix
			if !codexTurnMetadataOmitsField(canonical, field) {
				raw, _ = sjson.Set(raw, field, mapped)
			}
			for _, flat := range []string{field, codexOutboundFlatFields[field]} {
				if flat != "" && gjson.GetBytes(body, "client_metadata."+flat).Exists() {
					body, _ = sjson.SetBytes(body, "client_metadata."+flat, mapped)
				}
			}
		}
		if gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").IsObject() {
			body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(raw))
		} else {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", raw)
		}
		// The canonical snapshot is authoritative over all flat aliases.
		for field, flat := range codexOutboundFlatFields {
			if v := gjson.Get(raw, field); v.Exists() {
				body, _ = sjson.SetRawBytes(body, "client_metadata."+flat, []byte(v.Raw))
			}
		}
		if originalRequest != "" {
			mappedRequest := gjson.Get(raw, "client_request_id").String()
			if mappedRequest == "" {
				mappedRequest = derive("identity", originalRequest)
			}
			body, _ = sjson.SetBytes(body, "client_metadata.x-client-request-id", mappedRequest)
			body, _ = sjson.SetBytes(body, "client_metadata.client_request_id", mappedRequest)
		}
		body, headers = FinalizeCodexOutboundMetadata(body, headers)
	}
	if cache := gjson.GetBytes(body, "prompt_cache_key"); cache.Type == gjson.String {
		body, _ = sjson.SetBytes(body, "prompt_cache_key", derive("cache", cache.String()))
	}
	// Do not introduce a provider-unsupported body carrier merely because the
	// caller sent headers. Existing capability fallback decides injection.
	if !hadMetadata {
		body, _ = sjson.DeleteBytes(body, "client_metadata")
	}
	for _, name := range []string{"OpenAI-Organization", "OpenAI-Project"} {
		deleteHeaderCaseInsensitive(headers, name)
	}
	if value := headers.Get("Idempotency-Key"); value != "" {
		headers.Set("Idempotency-Key", derive("idempotency", value))
	}
	return body, headers, nil
}
