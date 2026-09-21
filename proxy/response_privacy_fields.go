package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/codex2api/auth"
)

// Normalize protocol field names only, never values. This covers HTTP spelling,
// snake_case and camelCase without allowing a variant to escape the policy.
func privacyField(key string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(key))
}

func privateResponseField(key string) bool {
	switch privacyField(key) {
	case "session", "thread", "conversation", "account", "device", "installation", "project":
		return true
	case "guardianclassifiersourcethreadid", "traceparent", "tracestate", "wsrequestheadertraceparent", "wsrequestheadertracestate", "sessionid", "threadid", "conversationid", "parentthreadid", "forkedfromthreadid",
		"taskid", "contextwindowid", "turnid", "rootturnid", "agentname", "accountid", "chatgptaccountid", "organizationid",
		"organization", "projectid", "installationid", "deviceid", "windowid",
		"windownumber", "clientrequestid", "requestid", "traceid", "userid", "email", "accountemail",
		"authorization", "proxyauthorization", "cookie", "setcookie", "accesstoken", "refreshtoken",
		"idtoken", "apikey", "xapikey", "token", "bearertoken", "credential", "credentials", "password", "secret",
		"openaiorganization", "openaiproject", "xrequestid", "xopenairequestid", "xclientrequestid",
		"xcodexinstallationid", "xcodexwindowid", "xcodexparentthreadid", "xcodexforkedfromthreadid",
		"xcodexcontextwindowid", "xcodexsessionid", "xcodexthreadid", "openaiorganizationid", "openaiprojectid", "promptcachekey", "safetyidentifier", "user":
		return true
	}
	return false
}

func responseBusinessField(key string) bool {
	switch privacyField(key) {
	case "input", "output", "content", "arguments", "tools", "toolchoice", "parameters", "schema", "text", "delta", "instructions", "audio", "image", "refusal":
		return true
	}
	return false
}

// These fields are model/tool payloads only at a protocol position. Callers must
// never apply this exemption inside metadata, errors or other control subtrees.
// In particular, response.output is an envelope, while tool-item output is data.
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

func responseOpaquePayloadField(kind, key string) bool { return ResponseOpaquePayloadField(kind, key) }

// Tool errors have their own schema. They are not request/transport errors.
func ResponseToolErrorField(kind, key string) bool {
	return privacyField(key) == "error" && (kind == "mcp_call" || kind == "mcp_list_tools")
}

type responsePrivacyWalker struct {
	ctx     context.Context
	account *auth.Account
}

// Protocol objects are traversed recursively. Only actual business payload
// positions are opaque; a key named "content" inside metadata is still metadata.
// JSON strings and arrays within control information use the same policy.
func (w responsePrivacyWalker) rewrite(raw json.RawMessage, responseObject, control, errorObject bool, depth int) (json.RawMessage, error) {
	if depth > 64 {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("invalid upstream response envelope")
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		if control {
			decoded := strings.TrimSpace(value)
			if strings.HasPrefix(decoded, "{") || strings.HasPrefix(decoded, "[") {
				if !json.Valid([]byte(decoded)) {
					return nil, nil
				}
				out, err := w.rewrite([]byte(decoded), false, true, errorObject, depth+1)
				if err != nil || out == nil {
					return out, err
				}
				return json.Marshal(string(out))
			}
		}
		if errorObject {
			return w.errorText(value)
		}
		if control {
			if db, binding := protocolIdentityBinding(w.ctx, w.account); db != nil {
				pair, found, err := db.ReadCodexProtocolPair(w.ctx, binding, "metadata", value, false)
				if err != nil {
					return nil, errTurnStateMapping
				}
				if found {
					return json.Marshal(pair.Public)
				}
			}
			if strings.HasPrefix(value, "meta_") {
				return nil, nil
			}
		}
		return raw, nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		for i, item := range items {
			out, err := w.rewrite(item, false, control, errorObject, depth+1)
			if err != nil {
				return nil, err
			}
			items[i] = out
		}
		return json.Marshal(items)
	}
	if trimmed[0] != '{' {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	var kind string
	_ = json.Unmarshal(object["type"], &kind)
	// Canonicalizing before inspection ensures duplicate keys use one meaning.
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if depth == 0 {
		// Establish the authoritative response ID before looking at optional
		// metadata references to it; diagnostics must not mint their own IDs.
		sort.SliceStable(keys, func(i, j int) bool { return privacyField(keys[i]) == "response" && privacyField(keys[j]) != "response" })
	}
	for _, key := range keys {
		value := object[key]
		field := privacyField(key)
		if !control && !errorObject && responseOpaquePayloadField(kind, key) {
			continue
		}
		var out json.RawMessage
		var err error
		switch {
		case field == "metadata" || field == "clientmetadata" || field == "xcodexturnmetadata":
			out, err = restoreCodexURLMetadata(w.ctx, w.account, value)
			if err == nil {
				out, err = w.rewrite(out, false, true, errorObject, depth+1)
			}
		case !control && !errorObject && key == "moderation":
			out, err = w.toolError(value)
		case !control && !errorObject && ResponseToolErrorField(kind, key):
			out, err = w.toolError(value)
		case !control && !errorObject && field == "internalchatmessagemetadatapassthrough":
			out, err = w.itemMetadata(value)
		case responseObject && !errorObject && key == "conversation":
			out, err = w.conversation(value)
		case privateResponseField(key):
			delete(object, key)
			continue
		case field == "xcodexturnstate":
			if !errorObject {
				out, err = rewriteTurnStateValue(value, "response_metadata."+key, func(real, carrier string) (string, error) {
					return maskResponseTurnState(w.ctx, w.account, real, carrier)
				})
			}
		case field == "headers":
			out, err = w.headers(value, errorObject, depth+1)
		case field == "id" && responseObject || field == "responseid" || field == "previousresponseid" || field == "comparisonresponseid" || field == "parentresponseid":
			out, err = w.reference(value, field != "comparisonresponseid" && field != "parentresponseid" && (responseObject || depth == 0) && !errorObject)
		case !control && field == "output":
			// Output item IDs/arguments/content are business data, but an item's
			// metadata is still a protocol carrier and must not escape filtering.
			out, err = w.rewrite(value, false, false, errorObject, depth+1)
		default:
			childControl := control || (field != "response" && field != "item" && field != "part")
			childError := errorObject || field == "error"
			out, err = w.rewrite(value, field == "response" && depth == 0, childControl, childError, depth+1)
		}
		if err != nil {
			return nil, err
		}
		if out == nil {
			delete(object, key)
		} else {
			object[key] = out
		}
	}
	return json.Marshal(object)
}

func (w responsePrivacyWalker) headers(raw json.RawMessage, inError bool, depth int) (json.RawMessage, error) {
	if depth > 64 {
		return []byte(`{}`), nil
	}
	var headers map[string]json.RawMessage
	if json.Unmarshal(raw, &headers) != nil || headers == nil {
		var encoded string
		if json.Unmarshal(raw, &encoded) == nil {
			out, err := w.headers([]byte(encoded), inError, depth+1)
			if err != nil {
				return nil, err
			}
			return json.Marshal(string(out))
		}
		return []byte(`{}`), nil
	}
	for key, value := range headers {
		switch privacyField(key) {
		case "xcodexturnstate":
			if inError {
				delete(headers, key)
				continue
			}
			out, err := rewriteTurnStateValue(value, "response_headers."+key, func(real, carrier string) (string, error) {
				return maskResponseTurnState(w.ctx, w.account, real, carrier)
			})
			if err != nil {
				return nil, err
			}
			if out == nil {
				delete(headers, key)
			} else {
				headers[key] = out
			}
		case "openaimodel", "xopenaimodel", "xreasoningincluded":
			// Header values must be strings (or arrays of strings), not a hidden
			// dictionary bypassing the envelope traversal.
			var scalar string
			var values []string
			if json.Unmarshal(value, &scalar) != nil && json.Unmarshal(value, &values) != nil {
				delete(headers, key)
			}
		default:
			delete(headers, key)
		}
	}
	return json.Marshal(headers)
}

func (w responsePrivacyWalker) reference(raw json.RawMessage, canIssue bool) (json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return raw, nil
	}
	var real string
	if json.Unmarshal(raw, &real) != nil {
		return nil, errors.New("invalid upstream response ID")
	}
	if real == "" {
		return raw, nil
	}
	state := responseIdentityFrom(w.ctx)
	if state == nil {
		// API relay has its own continuation semantics; native requests must
		// not silently disclose a raw ID when their mapping context is absent.
		if w.account != nil && w.account.IsRelayStyle() {
			return raw, nil
		}
		return nil, nil
	}
	if canIssue {
		alias, err := state.issue(w.ctx, w.account, real)
		if err != nil {
			return nil, err
		}
		return json.Marshal(alias)
	}
	// A nested diagnostic/echo is not authority to mint a continuation handle.
	alias, err := state.publicErrorReference(w.ctx, w.account, real)
	if err != nil {
		return nil, err
	}
	if alias == "[response]" {
		return nil, nil
	}
	return json.Marshal(alias)
}

func (w responsePrivacyWalker) errorText(original string) (json.RawMessage, error) {
	state := responseIdentityFrom(w.ctx)
	var mappingErr error
	masked := responseIDInError.ReplaceAllStringFunc(original, func(real string) string {
		if state == nil {
			return "[response]"
		}
		alias, err := state.publicErrorReference(w.ctx, w.account, real)
		if err != nil {
			mappingErr = err
		}
		return alias
	})
	if mappingErr != nil {
		return nil, mappingErr
	}
	if masked != original && state != nil {
		state.log(responseIdentityEvent{Action: "masked_error_reference", OriginalError: original})
	}
	return json.Marshal(masked)
}
