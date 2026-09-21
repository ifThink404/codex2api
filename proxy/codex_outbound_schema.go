package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeOutboundMetadataScalar(field string, raw json.RawMessage) json.RawMessage {
	v := gjson.ParseBytes(raw)
	if v.Type != gjson.String {
		return raw
	}
	switch field {
	case "window_number", "turn_started_at_unix_ms", "forked_from_ordinal_exclusive":
		if number, err := strconv.ParseUint(v.String(), 10, 64); err == nil {
			return json.RawMessage(strconv.FormatUint(number, 10))
		}
	case "analytics_enabled", "auto_review_enabled", "node_repl_auto_review_required", "node_repl_disabled", "history_ingest_requested":
		if v.String() == "true" || v.String() == "false" {
			return json.RawMessage(v.String())
		}
	}
	return raw
}

// Match TurnToolNamespacesInfo in codex-rs. Names are executable references,
// including MCP server names; changing them would break tool dispatch.
func sanitizeCodexToolInventory(raw json.RawMessage) json.RawMessage {
	type source struct {
		Kind       string `json:"kind"`
		ServerName string `json:"server_name,omitempty"`
	}
	type function struct {
		Name         string  `json:"name"`
		Direct       bool    `json:"direct"`
		CodeModeName *string `json:"code_mode_name"`
		Deferred     bool    `json:"deferred"`
		Source       source  `json:"source"`
	}
	type namespace struct {
		Name      string              `json:"name"`
		Functions map[string]function `json:"functions"`
	}
	var inventory map[string]namespace
	if json.Unmarshal(raw, &inventory) != nil || inventory == nil {
		return nil
	}
	for key, ns := range inventory {
		if ns.Functions == nil {
			ns.Functions = make(map[string]function)
		}
		for name, fn := range ns.Functions {
			if fn.Source.Kind != "harness" && fn.Source.Kind != "mcp" {
				return nil
			}
			if fn.Source.Kind == "harness" {
				fn.Source.ServerName = ""
			}
			ns.Functions[name] = fn
		}
		inventory[key] = ns
	}
	out, _ := json.Marshal(inventory)
	return out
}

// Responses client_metadata is HashMap<String, String>. Only its encoded turn
// snapshot contains typed booleans, integers and nested structures.
func encodeCodexClientMetadata(body []byte) []byte {
	flat := gjson.GetBytes(body, "client_metadata")
	if !flat.IsObject() {
		return body
	}
	encoded := make(map[string]string)
	flat.ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.String {
			encoded[key.String()] = value.String()
		} else if value.Type != gjson.Null {
			encoded[key.String()] = value.Raw
		}
		return true
	})
	body, _ = sjson.SetBytes(body, "client_metadata", encoded)
	return body
}

// This key is persistent when the gateway identity store is available. Direct
// executor embeddings use selected-account credentials, never user credentials
// as the HMAC key. Caller identity scopes aliases but is never sent upstream.
func codexFunctionalAliasDeriver(ctx context.Context, account *auth.Account, headers http.Header, caller string) (func(string, string) string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	owner := verifiedTransportUser(ctx)
	if owner == "" {
		owner = caller
	}
	if owner == "" {
		owner = codexIdentityDigest("functional-caller", headers.Get("Authorization"))
	}
	base, credential := account.OpenAIResponsesCredentials()
	account.Mu().RLock()
	if credential == "" {
		credential = account.AccessToken
	}
	account.Mu().RUnlock()
	key := codexIdentityDigest("outbound-functional-v1", fmt.Sprint(account.ID()), account.EffectiveAccountID(), base, owner)
	secret := []byte(credential)
	if store, ok := ctx.Value(codexIdentityClaimerContextKey{}).(CodexIdentityStore); ok {
		policy, err := store.ResolveCodexIdentityMapping(ctx, key, nil, true)
		if err != nil {
			return nil, codexAccountIdentityError("暂时无法读取功能字段身份映射，请重试。")
		}
		secret, err = hex.DecodeString(policy.Secret)
		if err != nil || len(secret) != 32 {
			return nil, codexAccountIdentityError("功能字段身份映射密钥不可用。")
		}
	} else if len(secret) == 0 {
		return nil, codexAccountIdentityError("缺少可信的功能字段身份映射密钥。")
	}
	return func(domain, original string) string {
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(key + "\x00" + domain + "\x00" + original))
		return "out_" + hex.EncodeToString(mac.Sum(nil))[:40]
	}, nil
}

// Apply once, after account identity mapping and before final projection. The
// executor replays the resulting bytes; it never feeds aliases back in here.
func PrepareCodexFunctionalFields(ctx context.Context, account *auth.Account, body []byte, headers http.Header, caller string) ([]byte, error) {
	var err error
	body, err = prepareConversationOutbound(ctx, account, body)
	if err != nil {
		return nil, err
	}
	body, err = prepareCodexFunctionalContainers(body)
	if err != nil {
		return nil, err
	}
	// Recheck the normalized final container as well as the restart boundary:
	// duplicate-key canonicalization or later request rules may change a reference.
	body, err = prepareComparisonResponseIdentity(ctx, account, body)
	if err != nil {
		return nil, err
	}
	body, err = prepareParentResponseIdentity(ctx, account, body)
	if err != nil {
		return nil, err
	}
	body, err = prepareCodexAuxiliaryMetadata(ctx, account, body, headers, caller)
	if err != nil {
		return nil, err
	}
	// These restored fields belong to the public Responses API. The native
	// ChatGPT backend has a different contract (including no safety_identifier).
	if account != nil && !account.IsRelayStyle() {
		for _, field := range []string{"metadata", "moderation", "user", "safety_identifier"} {
			body, _ = sjson.DeleteBytes(body, field)
		}
	}
	metadata := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
	if !gjson.GetBytes(body, "user").Exists() && !gjson.GetBytes(body, "safety_identifier").Exists() && !gjson.GetBytes(body, "metadata").Exists() && !metadata.Get("agent_name").Exists() {
		return body, nil
	}
	derive, err := codexFunctionalAliasDeriver(ctx, account, headers, caller)
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"user", "safety_identifier"} {
		if v := gjson.GetBytes(body, field); v.Type == gjson.String {
			body, _ = sjson.SetBytes(body, field, derive("subject", v.String()))
		} else if v.Exists() && v.Type != gjson.Null {
			return nil, codexAccountIdentityError("用户标识字段必须是字符串：" + field)
		}
	}
	if name := metadata.Get("agent_name"); name.Type == gjson.String {
		// Preserve /root and ancestor relationships, not user-chosen agent names.
		parts := strings.Split(strings.Trim(name.String(), "/"), "/")
		mapped := make([]string, len(parts))
		for i := range parts {
			if i == 0 && parts[i] == "root" {
				mapped[i] = "root"
			} else {
				mapped[i] = derive("agent", strings.Join(parts[:i+1], "/"))
			}
		}
		raw, _ := sjson.Set(metadata.Raw, "agent_name", "/"+strings.Join(mapped, "/"))
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", raw)
	}
	if business := gjson.GetBytes(body, "metadata"); business.IsObject() {
		clean := make(map[string]string)
		business.ForEach(func(key, value gjson.Result) bool {
			// Public metadata is a string map, not an alternate nested envelope.
			if value.Type != gjson.String || gjson.Valid(value.String()) && (gjson.Parse(value.String()).IsObject() || gjson.Parse(value.String()).IsArray()) {
				return true
			}
			field := outboundMetadataField(key.String())
			if field == "user" || field == "user_id" || field == "safety_identifier" {
				clean[key.String()] = derive("subject", value.String())
			} else if _, known := codexOutboundMetadataFields[field]; known {
				// The canonical transport snapshot wins over stale business copies.
				current := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")).Get(field)
				if current.Exists() {
					clean[key.String()] = current.String()
				}
			} else if privateResponseField(key.String()) || isTurnStateContainer(key.String()) || isTurnStateField(key.String()) || field == "authorization" || field == "x_oai_attestation" {
				// Credentials and arbitrary identity envelopes are not business labels.
			} else {
				clean[key.String()] = value.String()
			}
			return true
		})
		body, _ = sjson.SetBytes(body, "metadata", clean)
	} else if business.Exists() && business.Type != gjson.Null {
		return nil, codexAccountIdentityError("metadata 必须是字符串键值对象。")
	}
	return body, nil
}

func prepareCodexFunctionalContainers(body []byte) ([]byte, error) {
	// Only protocol controls are traversed. Business input, schemas and tool
	// arguments may contain arbitrary objects and are deliberately untouched.
	var filter func(gjson.Result, map[string]any, string) (map[string]any, error)
	filter = func(object gjson.Result, fields map[string]any, path string) (map[string]any, error) {
		if !object.IsObject() {
			return nil, codexAccountIdentityError("功能参数必须是对象：" + path)
		}
		out := make(map[string]any)
		for field, shape := range fields {
			value := object.Get(field)
			if !value.Exists() {
				continue
			}
			if value.Type == gjson.Null {
				out[field] = nil
				continue
			}
			if nested, ok := shape.(map[string]any); ok {
				child, err := filter(value, nested, path+"."+field)
				if err != nil {
					return nil, err
				}
				out[field] = child
			} else if shape == "bool" && (value.Type == gjson.True || value.Type == gjson.False) {
				out[field] = value.Bool()
			} else if shape == "string" && value.Type == gjson.String {
				out[field] = value.String()
			} else {
				return nil, codexAccountIdentityError("功能参数类型不正确：" + path + "." + field)
			}
		}
		return out, nil
	}
	for name, fields := range map[string]map[string]any{
		"access_programs":      {"cyber": "string"},
		"prompt_cache_options": {"comparison_response_id": "string", "mode": "string", "prewarm": "bool", "ttl": "string"},
		"moderation":           {"model": "string", "policy": map[string]any{"input": map[string]any{"mode": "string"}, "output": map[string]any{"mode": "string"}}},
	} {
		value := gjson.GetBytes(body, name)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		clean, err := filter(value, fields, name)
		if err != nil {
			return nil, err
		}
		body, _ = sjson.SetBytes(body, name, clean)
	}
	if generate := gjson.GetBytes(body, "generate"); generate.Exists() && generate.Type != gjson.Null && generate.Type != gjson.True && generate.Type != gjson.False {
		return nil, codexAccountIdentityError("generate 必须是布尔值。")
	}
	return body, nil
}

// WS controls must never turn into a different operation on HTTP fallback.
func prepareCodexHTTPControls(body []byte) ([]byte, error) {
	if v := gjson.GetBytes(body, "generate"); v.Exists() && v.Type != gjson.Null && v.Type != gjson.True {
		return nil, &Error{Code: "websocket_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "generate=false 预热请求需要 WebSocket 上游，当前 HTTP 通道无法执行。"}
	}
	if v := gjson.GetBytes(body, "stream_id"); v.Exists() && v.Type != gjson.Null {
		return nil, &Error{Code: "websocket_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "stream_id 需要 WebSocket 上游，当前 HTTP 通道无法执行。"}
	}
	body, _ = sjson.DeleteBytes(body, "generate")
	body, _ = sjson.DeleteBytes(body, "stream_id")
	body, _ = sjson.DeleteBytes(body, "type")
	return body, nil
}

// Compact has its own schema. In particular it is not a create request with
// stream=false; tools, access_programs and client_metadata are not body fields.
func prepareCodexCompactFields(body []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	for key := range top {
		switch key {
		case "model", "input", "instructions", "previous_response_id", "prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "service_tier":
		default:
			delete(top, key)
		}
	}
	body, _ = json.Marshal(top)
	return body
}

func validCodexStreamID(original string) bool {
	valid := len(original) > 0 && len(original) <= 256
	for _, ch := range original {
		valid = valid && (ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.')
	}
	return valid
}

func PrepareCodexStreamID(ctx context.Context, account *auth.Account, body []byte, headers http.Header, caller string) ([]byte, string, string, error) {
	v := gjson.GetBytes(body, "stream_id")
	if !v.Exists() || v.Type == gjson.Null {
		return body, "", "", nil
	}
	original := v.String()
	valid := v.Type == gjson.String && validCodexStreamID(original)
	if !valid {
		return nil, "", "", codexAccountIdentityError("stream_id 必须为 1–256 位字母、数字、下划线、连字符或点。")
	}
	derive, err := codexFunctionalAliasDeriver(ctx, account, headers, caller)
	if err != nil {
		return nil, "", "", err
	}
	thread := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")).Get("thread_id").String()
	mapped := derive("stream:"+thread, original)
	body, err = sjson.SetBytes(body, "stream_id", mapped)
	return body, original, mapped, err
}
