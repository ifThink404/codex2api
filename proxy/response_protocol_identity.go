package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func protocolIdentityBinding(ctx context.Context, account *auth.Account) (*database.DB, database.CodexTurnStateBinding) {
	var binding database.CodexTurnStateBinding
	if ctx == nil || account == nil {
		return nil, binding
	}
	s, _ := ctx.Value(protocolIdentityKey{}).(*responseIdentitySession)
	if s == nil {
		s = responseIdentityFrom(ctx)
	}
	if s == nil || s.handler == nil || s.handler.db == nil {
		return nil, binding
	}
	binding = database.CodexTurnStateBinding{Scope: s.scope, RootKey: s.rootKey, AccountID: account.ID(), AccountHash: turnStateAccountHash(account)}
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		binding.Generation = epoch.record.FailoverCount
	}
	return s.handler.db, binding
}

// Touch only item metadata at the documented history position. Function
// arguments, tool outputs and user-authored same-named keys remain opaque.
func rewriteHistoryTurnIDs(body []byte, rewrite func(string) string) []byte {
	for index, item := range gjson.GetBytes(body, "input").Array() {
		metadata, object := historyItemMetadata(item)
		v := gjson.ParseBytes(metadata["turn_id"])
		if v.Type != gjson.String {
			continue
		}
		if next := rewrite(v.String()); next != v.String() {
			metadata["turn_id"], _ = json.Marshal(next)
		}
		object["internal_chat_message_metadata_passthrough"], _ = json.Marshal(metadata)
		encoded, _ := json.Marshal(object)
		body, _ = sjson.SetRawBytes(body, fmt.Sprintf("input.%d", index), encoded)
	}
	return body
}

// JSON's last value wins in both collection and emission. Replacing the whole
// item prevents a second duplicate metadata/turn_id key surviving an sjson edit.
func historyItemMetadata(item gjson.Result) (map[string]json.RawMessage, map[string]json.RawMessage) {
	var object, metadata map[string]json.RawMessage
	_ = json.Unmarshal([]byte(item.Raw), &object)
	_ = json.Unmarshal(object["internal_chat_message_metadata_passthrough"], &metadata)
	return metadata, object
}

func (w responsePrivacyWalker) itemMetadata(raw json.RawMessage) (json.RawMessage, error) {
	value := gjson.ParseBytes(raw)
	if !value.IsObject() {
		return nil, nil
	}
	result := make(map[string]json.RawMessage)
	if v := value.Get("create_time"); v.Type == gjson.Number {
		result["create_time"] = json.RawMessage(v.Raw)
	}
	if v := value.Get("content_item_kinds"); v.IsArray() {
		valid := true
		for _, kind := range v.Array() {
			valid = valid && kind.Type == gjson.String
		}
		if valid {
			result["content_item_kinds"] = json.RawMessage(v.Raw)
		}
	}
	if v := value.Get("turn_id"); v.Type == gjson.String && v.String() != "" {
		var original string
		db, binding := protocolIdentityBinding(w.ctx, w.account)
		if db != nil {
			pair, found, err := readResponseProtocolPair(w.ctx, db, binding, "turn", v.String(), false)
			if err != nil {
				return nil, err
			}
			if found {
				original = pair.Public
			} else {
				pair, found, err = readResponseProtocolPair(w.ctx, db, binding, "turn", v.String(), true)
				if err != nil {
					return nil, err
				}
				if found {
					original = pair.Public
				} // already restored; known to this owner
			}
		}
		// Direct executor embeddings may not install the Handler's identity
		// context; only their verified outbound snapshot can restore a value.
		if original == "" && db == nil && w.account != nil {
			if d, _ := w.ctx.Value(codexAccountIdentityDiagnosticKey{}).(*codexAccountIdentityDiagnostic); d != nil && d.UpstreamAccount == diagnosticIdentifier(w.account.EffectiveAccountID()) {
				for _, change := range d.Changes {
					if change.Outbound != v.String() {
						continue
					}
					for _, field := range change.Fields {
						if strings.Contains(field, "turn_id") {
							original = change.Original
						}
					}
				}
			}
		}
		if original != "" {
			result["turn_id"], _ = json.Marshal(original)
		}
	}
	return json.Marshal(result)
}

func (w responsePrivacyWalker) conversation(raw json.RawMessage) (json.RawMessage, error) {
	if string(raw) == "null" {
		return raw, nil
	}
	value := gjson.ParseBytes(raw)
	id := value.Get("id")
	if !value.IsObject() || id.Type != gjson.String || id.String() == "" || len(id.String()) > 256 {
		return nil, nil
	}
	public := id.String()
	if db, binding := protocolIdentityBinding(w.ctx, w.account); db != nil {
		if db.IsManagedCodexConversationAlias(id.String()) {
			pair, found, err := readResponseProtocolPair(w.ctx, db, binding, "conversation", id.String(), true)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, responseMappingFailure(w.ctx, "conversation_lookup", nil, time.Time{})
			}
			return json.Marshal(map[string]string{"id": pair.Public})
		}
		public = db.CodexConversationAlias(binding, id.String())
		if err := putResponseProtocolPair(w.ctx, db, binding, "conversation", database.CodexProtocolPair{Public: public, Upstream: id.String()}); err != nil {
			return nil, err
		}
	} else if w.account == nil || !w.account.IsRelayStyle() {
		return nil, nil
	}
	return json.Marshal(map[string]string{"id": public})
}

func conversationReferenceID(value gjson.Result) (string, bool) {
	if value.IsObject() {
		var id gjson.Result
		count := 0
		value.ForEach(func(key, child gjson.Result) bool {
			if key.String() == "id" {
				id, count = child, count+1
			}
			return true
		})
		if count != 1 {
			return "", false
		}
		value = id
	}
	return value.String(), value.Type == gjson.String && value.String() != "" && len(value.String()) <= 256
}

func prepareConversationOutbound(ctx context.Context, account *auth.Account, body []byte) ([]byte, error) {
	v := gjson.GetBytes(body, "conversation")
	if !v.Exists() || v.Type == gjson.Null {
		return body, nil
	}
	path := "conversation"
	if v.IsObject() {
		path += ".id"
	}
	id, valid := conversationReferenceID(v)
	if !valid {
		return nil, codexAccountIdentityError("对话句柄格式无效，请重新发起请求。")
	}
	db, binding := protocolIdentityBinding(ctx, account)
	if db == nil {
		return body, nil
	}
	pair, found, err := readResponseProtocolPair(ctx, db, binding, "conversation", id, true)
	if err != nil {
		return nil, err
	}
	if found {
		return sjson.SetBytes(body, path, pair.Upstream)
	}
	if db.IsManagedCodexConversationAlias(id) {
		return nil, codexAccountIdentityError("对话句柄不属于当前账号或会话，无法继续，请新建对话。")
	}
	// Public API callers may already own a conversation created through its
	// resource endpoint. Opaque upstream handles cannot be arbitrarily remapped.
	return body, nil
}

// Both directions can occur at executor boundaries. Authenticate either against
// the same user/root/account/generation mapping that issued the public handle;
// a known token in another segment must never authorize this continuation.
func trustedConversationIdentity(ctx context.Context, record database.SessionContinuityRecord, value string) bool {
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || epoch.handler == nil || epoch.handler.store == nil || value == "" || len(value) > 256 {
		return false
	}
	account := epoch.handler.store.FindByID(record.AccountID)
	db, binding := protocolIdentityBinding(ctx, account)
	if db == nil || binding.AccountID != record.AccountID || binding.Generation != record.FailoverCount {
		return false
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, found, err := db.ReadCodexProtocolPair(lookup, binding, "conversation", value, true)
	if err != nil || found {
		return err == nil && found
	}
	_, found, err = db.ReadCodexProtocolPair(lookup, binding, "conversation", value, false)
	return err == nil && found
}
