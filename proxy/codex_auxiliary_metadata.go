package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexFlatControlFields = []string{"parent_response_id", "guardian_credits_requested", "ws_request_header_traceparent", "ws_request_header_tracestate", "x-codex-ws-stream-request-start-ms"}

func codexGuardianMode(headers http.Header, metadata gjson.Result) string {
	mode := strings.ToLower(strings.TrimSpace(headers.Get("X-Codex-Guardian")))
	source := metadata.Get("thread_source").String()
	if mode == "reviewer" && source == "guardian_review" || mode == "classifier" && source == "guardian_classifier" {
		return mode
	}
	return ""
}

func codexCredentialMetadataField(key string) bool {
	switch privacyField(key) {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "accesstoken", "refreshtoken", "idtoken", "apikey", "xapikey", "token", "bearertoken", "credential", "credentials", "password", "secret", "attestation", "xoaiattestation":
		return true
	}
	return false
}

// Official extra metadata is a string map. Preserve that contract without
// allowing it to supply credentials, alternate control containers or carriers.
func codexExtraMetadataField(key string, value gjson.Result) bool {
	if len(key) == 0 || len(key) > 64 || value.Type != gjson.String || len(value.String()) > 1024 || codexCredentialMetadataField(key) {
		return false
	}
	for i, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || i > 0 && (ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.')) {
			return false
		}
	}
	field := outboundMetadataField(key)
	if _, known := codexOutboundMetadataFields[field]; known {
		return false
	}
	if isTurnStateContainer(key) || isTurnStateField(key) || projectControlField(key) != "" {
		return false
	}
	for _, flat := range codexFlatControlFields {
		if field == outboundMetadataField(flat) {
			return false
		}
	}
	switch field {
	case "subagent_header", "memgen_request", "guardian", "workspaces", "compaction", "tool_namespaces_info", "code_mode_tool_names", "ws_request_header_x_openai_internal_codex_responses_lite":
		return false
	}
	return true
}

func codexExtensionIdentity(key, value string) bool {
	field := privacyField(key)
	// Split camelCase as well as snake/kebab keys; "valid" is not an ID.
	var label strings.Builder
	for i, ch := range key {
		if i > 0 && unicode.IsUpper(ch) {
			label.WriteByte('_')
		}
		label.WriteRune(unicode.ToLower(ch))
	}
	parts := strings.FieldsFunc(label.String(), func(ch rune) bool { return ch == '_' || ch == '-' || ch == '.' })
	last := ""
	if len(parts) > 0 {
		last = parts[len(parts)-1]
	}
	if privateResponseField(key) || last == "id" || last == "uuid" || last == "hash" || last == "path" || last == "url" || last == "email" || field == "hostname" || field == "username" || field == "machine" {
		return true
	}
	if _, err := uuid.Parse(value); err == nil {
		return true
	}
	if u, err := url.Parse(value); err == nil && u.Scheme != "" {
		return true
	}
	return strings.HasPrefix(value, "/") || strings.Contains(value, "@") || strings.Contains(value, `:\`)
}

type codexAuxiliaryIdentityKey struct{}

// Relay accounts do not have a native fingerprint diagnostic. Remember their
// authoritative before/after projection for extensions that repeat the same ID.
func withCodexAuxiliaryIdentities(ctx context.Context, before, after []byte) context.Context {
	aliases := make(map[string]string)
	oldFlat, newFlat := gjson.GetBytes(before, "client_metadata"), gjson.GetBytes(after, "client_metadata")
	oldMeta, newMeta := diagnosticMetadataObject(oldFlat.Get("x-codex-turn-metadata")), diagnosticMetadataObject(newFlat.Get("x-codex-turn-metadata"))
	for field := range codexOutboundMetadataFields {
		for _, objects := range [][2]gjson.Result{{oldMeta, newMeta}, {oldFlat, newFlat}} {
			a, b := objects[0].Get(field), objects[1].Get(field)
			if a.Type == gjson.String && b.Type == gjson.String && a.String() != b.String() {
				aliases[a.String()] = b.String()
			}
		}
	}
	return context.WithValue(ctx, codexAuxiliaryIdentityKey{}, aliases)
}

// Keep an account-scoped reversible mapping for extra identity values. This
// also makes repeated processing and response echoes safe across restarts.
func mapCodexExtraValue(ctx context.Context, account *auth.Account, key, original string, derive func(string, string) string) (string, error) {
	if original == "" {
		return original, nil
	}
	db, binding := protocolIdentityBinding(ctx, account)
	// Project values may already have been rewritten by the earlier project
	// pass. Its mapping is authoritative, including copies in extra metadata.
	if project := projectIdentityFrom(ctx); project != nil && project.active && project.binding.AccountID == account.ID() && project.binding.AccountHash == turnStateAccountHash(account) {
		project.mu.Lock()
		mapped, originalKnown := project.forward[original]
		_, aliasKnown := project.reverse[original]
		project.mu.Unlock()
		if originalKnown {
			return mapped, nil
		}
		if aliasKnown {
			return original, nil
		}
	}
	if db != nil {
		for _, public := range []bool{false, true} {
			pair, found, err := db.ReadCodexProtocolPair(ctx, binding, "metadata", original, public)
			if err != nil {
				return "", errTurnStateMapping
			}
			if found {
				return pair.Upstream, nil
			}
		}
	}
	mapped := original
	if state := responseIdentityFrom(ctx); state != nil {
		for _, reference := range []*database.CodexResponseIDRecord{state.incoming, state.comparison, state.parent} {
			if reference != nil && reference.CodexTurnStateBinding == binding {
				if original == reference.Real {
					return original, nil
				}
				if original == reference.Alias {
					mapped = reference.Real
					break
				}
			}
		}
	}
	if aliases, _ := ctx.Value(codexAuxiliaryIdentityKey{}).(map[string]string); aliases != nil {
		if value, ok := aliases[original]; ok {
			mapped = value
		}
		for _, value := range aliases {
			if value == original {
				return original, nil
			}
		}
	}
	if d, _ := ctx.Value(codexAccountIdentityDiagnosticKey{}).(*codexAccountIdentityDiagnostic); d != nil && d.UpstreamAccount == diagnosticIdentifier(account.EffectiveAccountID()) {
		for _, change := range d.Changes {
			if change.Original == original {
				mapped = change.Outbound
				break
			}
			if change.Outbound == original {
				return original, nil
			}
		}
	}
	if mapped == original && codexExtensionIdentity(key, original) {
		mapped = "meta_" + strings.TrimPrefix(derive("metadata", original), "out_")
	}
	if mapped != original && db != nil {
		if err := db.PutCodexProtocolPair(ctx, binding, "metadata", database.CodexProtocolPair{Public: original, Upstream: mapped}); err != nil {
			return "", errTurnStateMapping
		}
	}
	return mapped, nil
}

func rewriteCodexExtraJSON(ctx context.Context, account *auth.Account, key, value string, derive func(string, string) string, depth int) (string, error) {
	parsed := gjson.Parse(value)
	if gjson.Valid(value) && (parsed.IsObject() || parsed.IsArray()) {
		if depth >= 32 {
			return "", codexAccountIdentityError("扩展元数据嵌套过深，无法安全改写。")
		}
		if parsed.IsObject() {
			out := map[string]json.RawMessage{}
			var failure error
			parsed.ForEach(func(k, v gjson.Result) bool {
				if codexCredentialMetadataField(k.String()) || isTurnStateField(k.String()) {
					return true
				}
				if v.Type == gjson.String || v.IsObject() || v.IsArray() || v.Type == gjson.Number && codexExtensionIdentity(k.String(), "") {
					next, err := rewriteCodexExtraJSON(ctx, account, k.String(), v.String(), derive, depth+1)
					if err != nil {
						failure = err
						return false
					}
					if !v.IsObject() && !v.IsArray() {
						out[k.String()], _ = json.Marshal(next)
					} else {
						out[k.String()] = json.RawMessage(next)
					}
				} else {
					out[k.String()] = json.RawMessage(v.Raw)
				}
				return true
			})
			if failure != nil {
				return "", failure
			}
			raw, err := json.Marshal(out)
			return string(raw), err
		}
		out := make([]json.RawMessage, 0)
		for _, v := range parsed.Array() {
			if v.Type == gjson.String || v.IsObject() || v.IsArray() || v.Type == gjson.Number && codexExtensionIdentity(key, "") {
				next, err := rewriteCodexExtraJSON(ctx, account, key, v.String(), derive, depth+1)
				if err != nil {
					return "", err
				}
				if !v.IsObject() && !v.IsArray() {
					raw, _ := json.Marshal(next)
					out = append(out, raw)
				} else {
					out = append(out, json.RawMessage(next))
				}
			} else {
				out = append(out, json.RawMessage(v.Raw))
			}
		}
		raw, err := json.Marshal(out)
		return string(raw), err
	}
	return mapCodexExtraValue(ctx, account, key, value, derive)
}

func prepareCodexAuxiliaryMetadata(ctx context.Context, account *auth.Account, body []byte, headers http.Header, caller string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil {
		return body, nil
	}
	flat := gjson.GetBytes(body, "client_metadata")
	canonical := diagnosticMetadataObject(flat.Get("x-codex-turn-metadata"))
	var extras map[string]json.RawMessage
	_ = json.Unmarshal([]byte(canonical.Raw), &extras)
	changedExtras := false
	var derive func(string, string) string
	getDeriver := func() error {
		if derive != nil {
			return nil
		}
		var err error
		derive, err = codexFunctionalAliasDeriver(ctx, account, headers, caller)
		return err
	}
	for key, raw := range extras {
		value := gjson.ParseBytes(raw)
		if !codexExtraMetadataField(key, value) {
			continue
		}
		if err := getDeriver(); err != nil {
			return nil, err
		}
		next, err := rewriteCodexExtraJSON(ctx, account, key, value.String(), derive, 0)
		if err != nil {
			return nil, err
		}
		extras[key], _ = json.Marshal(next)
		changedExtras = changedExtras || next != value.String()
	}
	if canonical.IsObject() && changedExtras {
		raw, _ := json.Marshal(extras)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", string(raw))
	}
	// Unknown flat strings are also legal client metadata. A matching nested
	// extension is authoritative, but an absent nested field stays absent.
	var flatObject map[string]json.RawMessage
	_ = json.Unmarshal([]byte(gjson.GetBytes(body, "client_metadata").Raw), &flatObject)
	for key, raw := range flatObject {
		value := gjson.ParseBytes(raw)
		if !codexExtraMetadataField(key, value) {
			continue
		}
		if existing, ok := extras[key]; ok {
			flatObject[key] = existing
			continue
		}
		if err := getDeriver(); err != nil {
			return nil, err
		}
		next, err := rewriteCodexExtraJSON(ctx, account, key, value.String(), derive, 0)
		if err != nil {
			return nil, err
		}
		flatObject[key], _ = json.Marshal(next)
	}
	if flatObject != nil {
		raw, _ := json.Marshal(flatObject)
		body, _ = sjson.SetRawBytes(body, "client_metadata", raw)
	}
	// Tracing uses W3C lengths, not session UUID syntax. The same trace/span
	// values map identically across HTTP headers and WS frame metadata.
	for _, name := range []string{"traceparent", "tracestate"} {
		path := "client_metadata.ws_request_header_" + name
		v := gjson.GetBytes(body, path)
		value := headers.Get(name)
		if v.Exists() {
			value = v.String()
		}
		if value == "" {
			if v.Exists() {
				deleteHeaderCaseInsensitive(headers, name)
				body, _ = sjson.DeleteBytes(body, path)
			}
			continue
		}
		db, binding := protocolIdentityBinding(ctx, account)
		if db != nil {
			pair, found, err := db.ReadCodexProtocolPair(ctx, binding, "metadata", value, false)
			if err != nil {
				return nil, errTurnStateMapping
			}
			if found {
				if headers != nil {
					headers.Set(name, pair.Upstream)
				}
				if v.Exists() {
					body, _ = sjson.SetBytes(body, path, pair.Upstream)
				}
				continue
			}
		}
		if err := getDeriver(); err != nil {
			return nil, err
		}
		mapped := ""
		if name == "traceparent" {
			parts := strings.Split(strings.ToLower(value), "-")
			if len(parts) == 4 && parts[0] == "00" && len(parts[1]) == 32 && len(parts[2]) == 16 && len(parts[3]) == 2 {
				_, e1 := hex.DecodeString(parts[1] + parts[2] + parts[3])
				if e1 == nil && parts[1] != strings.Repeat("0", 32) && parts[2] != strings.Repeat("0", 16) {
					mapped = "00-" + strings.TrimPrefix(derive("trace", parts[1]), "out_")[:32] + "-" + strings.TrimPrefix(derive("span", parts[2]), "out_")[:16] + "-" + parts[3]
				}
			}
		} else if len(value) <= 512 {
			var members []string
			seen := make(map[string]bool)
			for _, member := range strings.Split(value, ",") {
				key, val, ok := strings.Cut(strings.TrimSpace(member), "=")
				if ok && validCodexTraceStateKey(key) && !seen[key] && len(members) < 32 {
					seen[key] = true
					mappedKey := key
					if tenant, vendor, multi := strings.Cut(key, "@"); multi {
						mappedKey = strings.TrimPrefix(derive("trace-tenant", tenant), "out_")[:12] + "@" + vendor
					}
					next := mappedKey + "=" + strings.TrimPrefix(derive("tracestate:"+key, val), "out_")
					if len(strings.Join(append(members, next), ",")) <= 512 {
						members = append(members, next)
					}
				}
			}
			mapped = strings.Join(members, ",")
		}
		if mapped != "" && mapped != value && db != nil {
			if err := db.PutCodexProtocolPair(ctx, binding, "metadata", database.CodexProtocolPair{Public: value, Upstream: mapped}); err != nil {
				return nil, errTurnStateMapping
			}
		}
		if headers != nil {
			headers.Del(name)
			if mapped != "" {
				headers.Set(name, mapped)
			}
		}
		if v.Exists() {
			body, _ = sjson.DeleteBytes(body, path)
			if mapped != "" {
				body, _ = sjson.SetBytes(body, path, mapped)
			}
		}
	}
	if v := gjson.GetBytes(body, "client_metadata.x-codex-ws-stream-request-start-ms"); v.Exists() {
		if n, err := strconv.ParseInt(v.String(), 10, 64); err != nil || n <= 0 {
			body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-ws-stream-request-start-ms")
		}
	}
	return body, nil
}

func validCodexTraceStateKey(key string) bool {
	valid := func(value string, limit int, digitFirst bool) bool {
		if len(value) == 0 || len(value) > limit {
			return false
		}
		for i, ch := range value {
			if ch >= 'a' && ch <= 'z' || (i > 0 || digitFirst) && ch >= '0' && ch <= '9' || i > 0 && strings.ContainsRune("_-*/", ch) {
				continue
			}
			return false
		}
		return true
	}
	if tenant, vendor, multi := strings.Cut(key, "@"); multi {
		return valid(tenant, 241, true) && valid(vendor, 14, false)
	}
	return valid(key, 256, false)
}
