package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const sessionContextProvenanceNamespace = "codex-session-context-v1"

type sessionContextTokenVerifier func(string, string) bool

func sessionContextScope(owner, rootKey, upstreamAccount string, record database.SessionContinuityRecord) string {
	if owner == "" || rootKey == "" || upstreamAccount == "" || record.AccountID <= 0 || record.FailoverCount == 0 {
		return ""
	}
	return codexIdentityDigest("codex-session-context-v1", owner, rootKey, upstreamAccount, strconv.FormatInt(record.AccountID, 10), strconv.FormatUint(record.FailoverCount, 10))
}

func sessionContextTokenKey(scope, kind, value string) string {
	return codexIdentityDigest(scope, kind, value)
}

func (handler *Handler) sessionContextVerifier(request *gin.Context, record database.SessionContinuityRecord, rootKeys ...string) (sessionContextTokenVerifier, context.CancelFunc) {
	scope := ""
	if handler.store != nil && len(rootKeys) > 0 {
		if account := handler.store.FindByID(record.AccountID); account != nil {
			scope = sessionContextScope(responseCacheOwnerForRequest(request, requestAPIKeyID(request)), hashRiskIdentity(rootKeys[0]), account.EffectiveAccountID(), record)
		}
	}
	known, cancel := handler.sessionContextVerifierForScope(request.Request.Context(), scope)
	return func(kind, value string) bool {
		if kind == "conversation" {
			return trustedConversationIdentity(request.Request.Context(), record, value)
		}
		if kind == "previous_response_id" && trustedResponseIdentity(request.Request.Context(), record, value) {
			return true
		}
		return kind == "turn_state" && trustedMappedTurnState(request.Request.Context(), record, value) || known(kind, value)
	}, cancel
}

func (handler *Handler) sessionContextVerifierForScope(ctx context.Context, scope string) (sessionContextTokenVerifier, context.CancelFunc) {
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	checked := make(map[string]bool)
	return func(kind, value string) bool {
		if scope == "" || strings.TrimSpace(value) == "" {
			return false
		}
		key := sessionContextTokenKey(scope, kind, value)
		if allowed, found := checked[key]; found {
			return allowed
		}
		if len(checked) >= 4096 {
			return false
		}
		allowed := false
		if handler.cache != nil {
			payload, found, err := handler.cache.GetRuntime(lookup, sessionContextProvenanceNamespace, key)
			allowed = err == nil && found && string(payload) == "1"
		}
		if !allowed && handler.db != nil {
			found, err := handler.db.HasSessionContextToken(lookup, key)
			allowed = err == nil && found
		}
		checked[key] = allowed
		return allowed
	}, cancel
}

func recordSessionContextTokens(ctx context.Context, account *auth.Account, collect func(func(string, string))) {
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || epoch.preview || epoch.temporary || epoch.handler == nil || account == nil || epoch.record.AccountID != account.ID() {
		return
	}
	scope := sessionContextScope(epoch.owner, epoch.key, epoch.upstreamAccount, epoch.record)
	if scope == "" {
		return
	}
	recording, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	written := make(map[string]time.Time)
	collect(func(kind, value string) {
		if strings.TrimSpace(value) == "" || recording.Err() != nil || len(written) >= 4096 {
			return
		}
		key := sessionContextTokenKey(scope, kind, value)
		if _, found := written[key]; found {
			return
		}
		ttl := compactionProvenanceTTL()
		if kind == "turn_state" {
			ttl = codexTurnStateProvenanceTTL
		}
		written[key] = time.Now().Add(ttl)
	})
	if epoch.handler.db != nil {
		if err := epoch.handler.db.RecordSessionContextTokens(recording, written); err != nil {
			return
		}
	}
	if epoch.handler.cache != nil {
		for key, expiry := range written {
			_ = epoch.handler.cache.SetRuntime(recording, sessionContextProvenanceNamespace, key, []byte("1"), time.Until(expiry))
		}
	}
}

func recordSessionTurnState(ctx context.Context, account *auth.Account, token string) {
	var db *database.DB
	if s := turnStateSessionFrom(ctx); s != nil && s.handler != nil {
		db = s.handler.db
	} else if epoch := outboundEpochFromContext(ctx); epoch != nil && epoch.handler != nil {
		db = epoch.handler.db
	}
	if db.IsManagedCodexTurnStateAlias(token) {
		return
	}
	if token != "" && !strings.HasPrefix(token, "c2ts_v1_") {
		recordSessionContextTokens(ctx, account, func(record func(string, string)) { record("turn_state", token) })
	}
}

func (handler *Handler) recordResponseContextProvenance(request *gin.Context, account *auth.Account, payload []byte) {
	handler.recordCompactionProvenanceFromPayload(context.Background(), account, payload)
	if request == nil || !gjson.ValidBytes(payload) {
		return
	}
	recordSessionContextTokens(request.Request.Context(), account, func(record func(string, string)) {
		root := gjson.ParseBytes(payload)
		inspect := func(item gjson.Result) {
			if !item.IsObject() || item.Get("type").String() == "" {
				return
			}
			if item.Get("type").String() == "reasoning" || gjsonResultHasEncryptedCompaction(item) {
				record("encrypted_content", item.Get("encrypted_content").String())
			}
			record("item_reference", item.Get("id").String())
		}
		if root.Get("type").String() == "response.output_item.done" || root.Get("type").String() == "response.output_item.added" {
			inspect(root.Get("item"))
		}
		if gjsonResultHasEncryptedCompaction(root) {
			inspect(root)
		}
		for _, path := range []string{"output", "response.output"} {
			for _, item := range root.Get(path).Array() {
				inspect(item)
			}
		}
	})
}

func sessionFailoverContextBlockWithVerifier(headers http.Header, body []byte, known sessionContextTokenVerifier) string {
	reason, _ := inspectSessionFailoverContext(headers, body, known)
	return reason
}

func inspectSessionFailoverContext(headers http.Header, body []byte, known sessionContextTokenVerifier) (string, []database.SessionContextBlocker) {
	for _, path := range []string{"previous_response_id", "conversation", "conversation_id"} {
		if value := gjson.GetBytes(body, path); value.Exists() && value.Type != gjson.Null && value.String() != "" {
			return "upstream_continuation", []database.SessionContextBlocker{{Kind: path, Path: path}}
		}
	}
	allowed := func(kind, value string) bool { return known != nil && known(kind, value) }
	statePath := ""
	_, _ = rewriteRequestTurnState(body, headers, func(token, carrier string) string {
		if statePath == "" && !allowed("turn_state", token) {
			statePath = carrier
		}
		return token
	})
	if statePath != "" {
		return "connection_turn_state", []database.SessionContextBlocker{{Kind: "turn_state", Path: statePath}}
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || input.Type == gjson.Null || input.IsArray() && len(input.Array()) == 0 {
		return "missing_request_context", []database.SessionContextBlocker{{Kind: "missing_input", Path: "input"}}
	}
	var blockers []database.SessionContextBlocker
	var inspect func(gjson.Result, string, string)
	inspect = func(value gjson.Result, path, itemType string) {
		if len(blockers) >= 8 || !value.IsArray() && !value.IsObject() {
			return
		}
		if value.IsObject() && value.Get("type").Exists() {
			itemType = "unknown"
			switch candidate := value.Get("type").String(); candidate {
			case "compaction", "reasoning", "message", "input_file", "input_image", "item_reference", "function_call", "function_call_output":
				itemType = candidate
			}
		}
		if value.IsObject() && value.Get("type").String() == "item_reference" && !allowed("item_reference", value.Get("id").String()) {
			blockers = append(blockers, database.SessionContextBlocker{Kind: "item_reference", Path: path + ".id", ItemType: itemType})
			return
		}
		for _, field := range []string{"encrypted_content", "file_id"} {
			token := value.Get(field)
			if len(blockers) < 8 && token.Exists() && token.Type != gjson.Null && token.String() != "" && !allowed(field, token.String()) {
				blockers = append(blockers, database.SessionContextBlocker{Kind: field, Path: path + "." + field, ItemType: itemType})
			}
		}
		if value.IsArray() {
			for index, item := range value.Array() {
				inspect(item, path+"["+strconv.Itoa(index)+"]", itemType)
			}
		} else {
			for _, field := range responseContextChildFields(value) {
				inspect(value.Map()[field], path+"."+responseContextSafeField(field), itemType)
			}
		}
	}
	inspect(input, "input", "")
	if len(blockers) > 0 {
		return "opaque_upstream_context", blockers
	}
	if index := missingToolSearchCall(input); index >= 0 {
		return "incomplete_tool_context", []database.SessionContextBlocker{{Kind: "missing_tool_call", Path: "input[" + strconv.Itoa(index) + "].call_id"}}
	}
	calls := make(map[string]bool)
	for _, item := range input.Array() {
		if strings.HasSuffix(item.Get("type").String(), "_call") {
			calls[item.Get("call_id").String()] = true
		}
	}
	for index, item := range input.Array() {
		if strings.HasSuffix(item.Get("type").String(), "_call_output") && !responseContextStandaloneFunctionOutput(item) && !calls[item.Get("call_id").String()] {
			return "incomplete_tool_context", []database.SessionContextBlocker{{Kind: "missing_tool_call", Path: "input[" + strconv.Itoa(index) + "].call_id"}}
		}
	}
	return "", nil
}
