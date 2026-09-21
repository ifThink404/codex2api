package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type responseIdentityKey struct{}
type protocolIdentityKey struct{}
type responseIdentityEvent = database.ResponseIdentityEvent
type responseIdentitySession struct {
	handler                     *Handler
	owner, scope, root, rootKey string
	mu                          sync.Mutex
	incoming                    *database.CodexResponseIDRecord
	comparison                  *database.CodexResponseIDRecord
	parent                      *database.CodexResponseIDRecord
	issued                      map[string]database.CodexResponseIDRecord
	events                      []responseIdentityEvent
}

func responseIdentityFrom(ctx context.Context) *responseIdentitySession {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(responseIdentityKey{}).(*responseIdentitySession)
	return s
}

func (h *Handler) bindResponseIdentity(c *gin.Context, identity requestSessionIdentity) {
	// API relay credentials have their own provider semantics; isolate native
	// Codex accounts here, just like the native executor response boundary.
	if h.db == nil {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), responseIdentityKey{}, (*responseIdentitySession)(nil)))
		return
	}
	owner := responseCacheOwnerForRequest(c, requestAPIKeyID(c))
	root := sessionAffinityKey(identity.affinityID, requestAPIKeyID(c))
	rootKey := hashRiskIdentity(root)
	if root == "" {
		rootKey = hashRiskIdentity("response-owner:" + owner)
	}
	s := &responseIdentitySession{handler: h, owner: owner, scope: codexIdentityDigest("response-id-owner-v1", owner), root: root, rootKey: rootKey, issued: make(map[string]database.CodexResponseIDRecord)}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), protocolIdentityKey{}, s))
	if apiRelaySessionExempt(c) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), responseIdentityKey{}, (*responseIdentitySession)(nil)))
		return
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), responseIdentityKey{}, s))
}

func (s *responseIdentitySession) log(event responseIdentityEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func responseIdentityDiagnostic(ctx context.Context) []responseIdentityEvent {
	s := responseIdentityFrom(ctx)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]responseIdentityEvent(nil), s.events...)
}

func invalidPreviousResponse() *Error {
	return &Error{Code: "previous_response_not_found", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "无法继续此响应，请提供完整对话历史后重试。"}
}

// Authenticate before consulting the context cache: a cache hit must never let
// an unverified ID bypass the owner/account checks. Keep aliases in local cache
// keys; restoration happens only at the final upstream executor boundary.
func validateResponseIdentityIngress(c *gin.Context, body []byte) error {
	s := responseIdentityFrom(c.Request.Context())
	value := gjson.GetBytes(body, "previous_response_id")
	if s == nil || !value.Exists() || value.Type == gjson.Null {
		return nil
	}
	id := value.String()
	if value.Type != gjson.String || id == "" || len(id) > 256 || strings.TrimSpace(id) != id {
		return invalidPreviousResponse()
	}
	reject := func(reason string) error {
		s.log(responseIdentityEvent{Action: reason, Received: id})
		return invalidPreviousResponse()
	}
	if s.handler.db == nil || s.handler.store == nil {
		return reject("rejected_store_unavailable")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
	defer cancel()
	record, found, err := s.handler.db.ReadCodexResponseID(ctx, id)
	if err != nil {
		return reject("rejected_store_unavailable")
	}
	managed := s.handler.db.IsManagedCodexResponseID(id)
	if managed && !found {
		return reject("rejected_unknown_or_expired")
	}
	var affinity responseAccountAffinity
	if !managed {
		var known bool
		affinity, known = lookupResponseAccountAffinity(ctx, s.handler.cache, s.owner, id)
		if known && affinity.LocalAlias {
			return reject("rejected_unverifiable_alias")
		}
		if !known || affinity.AffinityKey != s.root {
			return reject("rejected_unverified_legacy")
		}
		record = database.CodexResponseIDRecord{CodexTurnStateBinding: database.CodexTurnStateBinding{Scope: s.scope, RootKey: s.rootKey, AccountID: affinity.AccountID}, Real: id}
	}
	if record.Scope != s.scope || record.RootKey != s.rootKey {
		return reject("rejected_scope_mismatch")
	}
	owner, generation := record.AccountID, uint64(0)
	var currentRecord database.SessionContinuityRecord
	if s.root != "" {
		entry, known, readErr := s.handler.readSessionContinuity(ctx, s.rootKey)
		if readErr != nil {
			return reject("rejected_store_unavailable")
		}
		if known {
			owner, generation = entry.Record.AccountID, entry.Record.FailoverCount
			currentRecord = entry.Record
		} else {
			owner, _ = s.handler.store.LiveSessionAccountID(s.root, time.Now())
		}
	}
	account := s.handler.store.FindByID(owner)
	if account == nil || account.IsRelayStyle() || owner != record.AccountID {
		return reject("rejected_account_changed_or_missing")
	}
	if managed {
		if generation != record.Generation || record.AccountHash != turnStateAccountHash(account) {
			return reject("rejected_account_changed_or_missing")
		}
	} else {
		// Old raw IDs are accepted only with a verified current owner. After a
		// failover, require the recorded outbound segment as well (A -> B -> A).
		if generation > 0 {
			epoch := &sessionOutboundEpoch{key: s.rootKey, record: currentRecord}
			if segment := epoch.identityKey(); segment == "" || affinity.OutboundSegment != segment {
				return reject("rejected_legacy_generation")
			}
		}
		record.AccountHash, record.Generation = turnStateAccountHash(account), generation
	}
	s.incoming = &record
	action := "accepted_alias"
	if !managed {
		action = "accepted_verified_legacy"
	}
	s.log(responseIdentityEvent{Action: action, Received: id, Alias: record.Alias, Original: record.Real, AccountID: owner, Generation: generation})
	// Rebuild the short routing hint after a cache restart from authenticated
	// persistent state. It never changes the root's existing account binding.
	if managed {
		affinityContext := context.WithValue(ctx, sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{key: s.rootKey, record: currentRecord})
		s.handler.recordResponseAccountAffinity(s.owner, id, owner, s.root, gjson.GetBytes(body, "model").String(), responseAccountUpstreamType(account), affinityContext)
	}
	return nil
}

func prepareResponseIdentityOutbound(ctx context.Context, account *auth.Account, body []byte) (out []byte, err error) {
	defer func() {
		if err == nil {
			out, err = prepareComparisonResponseIdentity(ctx, account, out)
			if err == nil {
				out, err = prepareParentResponseIdentity(ctx, account, out)
			}
		}
	}()
	s := responseIdentityFrom(ctx)
	value := gjson.GetBytes(body, "previous_response_id")
	if s == nil || !value.Exists() || value.Type == gjson.Null {
		return body, nil
	}
	id := value.String()
	r := s.incoming
	generation := uint64(0)
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		generation = epoch.record.FailoverCount
	}
	if r == nil || (id != r.Alias && id != r.Real) || account == nil || account.IsRelayStyle() || r.AccountID != account.ID() || r.AccountHash != turnStateAccountHash(account) || r.Generation != generation {
		s.log(responseIdentityEvent{Action: "rejected_outbound_binding", Received: id})
		return nil, invalidPreviousResponse()
	}
	s.log(responseIdentityEvent{Action: "restored_outbound", Alias: r.Alias, Original: r.Real, AccountID: r.AccountID, Generation: r.Generation})
	return sjson.SetBytes(body, "previous_response_id", r.Real)
}

// A cache comparison is a reference, not a continuation/routing hint. Authenticate
// it against the selected account without replacing the previous-response record.
func prepareComparisonResponseIdentity(ctx context.Context, account *auth.Account, body []byte) ([]byte, error) {
	return prepareBoundResponseReference(ctx, account, body, "prompt_cache_options.comparison_response_id", false)
}

func prepareParentResponseIdentity(ctx context.Context, account *auth.Account, body []byte) ([]byte, error) {
	return prepareBoundResponseReference(ctx, account, body, "client_metadata.parent_response_id", true)
}

// Parent and comparison references never choose an account or replace the
// continuation binding. Resolve only after the selected account is known.
func prepareBoundResponseReference(ctx context.Context, account *auth.Account, body []byte, path string, parent bool) ([]byte, error) {
	s := responseIdentityFrom(ctx)
	v := gjson.GetBytes(body, path)
	if !v.Exists() || v.Type == gjson.Null || s == nil && (!parent || account != nil && account.IsRelayStyle()) {
		return body, nil
	}
	id := v.String()
	label := "cache_comparison"
	if parent {
		label = "parent_response"
	}
	reject := func() ([]byte, error) {
		if s != nil {
			s.log(responseIdentityEvent{Action: "rejected_" + label + "_binding", Received: id})
		}
		if parent {
			return nil, &Error{Code: "parent_response_not_found", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "父响应引用与当前账号或会话不匹配，请恢复主会话后重试。"}
		}
		return nil, &Error{Code: "comparison_response_not_found", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "缓存比较引用不可用，请移除 comparison_response_id 后重试。"}
	}
	if s == nil || v.Type != gjson.String || id == "" || len(id) > 256 || strings.TrimSpace(id) != id || account == nil || account.IsRelayStyle() || s.handler.db == nil {
		return reject()
	}
	generation := uint64(0)
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		generation = epoch.record.FailoverCount
	}
	var record database.CodexResponseIDRecord
	previous := s.comparison
	if parent {
		previous = s.parent
	}
	if r := previous; r != nil && (id == r.Alias || id == r.Real) {
		record = *r
	} else if r := s.incoming; r != nil && (id == r.Alias || id == r.Real) {
		record = *r
	} else {
		lookup, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var found bool
		var err error
		record, found, err = s.handler.db.ReadCodexResponseID(lookup, id)
		if err != nil {
			return reject()
		}
		if s.handler.db.IsManagedCodexResponseID(id) {
			if !found {
				return reject()
			}
		} else {
			affinity, known := lookupResponseAccountAffinity(lookup, s.handler.cache, s.owner, id)
			if !known || affinity.LocalAlias || affinity.AffinityKey != s.root || affinity.AccountID != account.ID() {
				return reject()
			}
			if generation > 0 {
				epoch := outboundEpochFromContext(ctx)
				if epoch == nil || epoch.identityKey() == "" || affinity.OutboundSegment != epoch.identityKey() {
					return reject()
				}
			}
			record = database.CodexResponseIDRecord{CodexTurnStateBinding: database.CodexTurnStateBinding{Scope: s.scope, RootKey: s.rootKey, AccountID: account.ID(), AccountHash: turnStateAccountHash(account), Generation: generation}, Real: id}
		}
	}
	if record.Scope != s.scope || record.RootKey != s.rootKey || record.AccountID != account.ID() || record.AccountHash != turnStateAccountHash(account) || record.Generation != generation {
		return reject()
	}
	if parent {
		s.parent = &record
	} else {
		s.comparison = &record
	}
	s.log(responseIdentityEvent{Action: "restored_" + label, Alias: record.Alias, Original: record.Real, AccountID: record.AccountID, Generation: record.Generation})
	return sjson.SetBytes(body, path, record.Real)
}

func trustedResponseIdentity(ctx context.Context, record database.SessionContinuityRecord, value string) bool {
	s := responseIdentityFrom(ctx)
	if s == nil || s.incoming == nil {
		return false
	}
	r := s.incoming
	return (value == r.Alias || value == r.Real) && r.AccountID == record.AccountID && r.Generation == record.FailoverCount
}

// Error messages may echo untrusted input. Quoting a response ID must never
// mint a new, authorized continuation handle for that ID.
func (s *responseIdentitySession) publicErrorReference(ctx context.Context, account *auth.Account, real string) (string, error) {
	if account == nil {
		return "[response]", nil
	}
	generation := uint64(0)
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		generation = epoch.record.FailoverCount
	}
	accountHash := turnStateAccountHash(account)
	matches := func(record database.CodexResponseIDRecord) bool {
		return record.Real == real && record.AccountID == account.ID() && record.AccountHash == accountHash && record.Generation == generation
	}
	s.mu.Lock()
	for _, record := range s.issued {
		if matches(record) {
			s.mu.Unlock()
			return record.Alias, nil
		}
	}
	s.mu.Unlock()
	if record := s.incoming; record != nil && matches(*record) {
		if record.Alias != "" {
			return record.Alias, nil
		}
		return s.issue(ctx, account, real)
	}
	for _, record := range []*database.CodexResponseIDRecord{s.comparison, s.parent} {
		if record != nil && matches(*record) {
			if record.Alias != "" {
				return record.Alias, nil
			}
			return s.issue(ctx, account, real)
		}
	}
	return "[response]", nil
}

func (s *responseIdentitySession) issue(ctx context.Context, account *auth.Account, real string) (string, error) {
	if real == "" {
		return "", nil
	}
	if s == nil || s.handler.db == nil || account == nil || account.IsRelayStyle() {
		return "", errTurnStateMapping
	}
	generation := uint64(0)
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		generation = epoch.record.FailoverCount
	}
	binding := database.CodexTurnStateBinding{Scope: s.scope, RootKey: s.rootKey, AccountID: account.ID(), AccountHash: turnStateAccountHash(account), Generation: generation}
	key := codexIdentityDigest(binding.AccountHash, real, strconv.FormatUint(generation, 10))
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.issued[key]; ok {
		return record.Alias, nil
	}
	if len(s.issued) >= 4096 {
		return "", errTurnStateMapping
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	record, err := s.handler.db.IssueCodexResponseID(lookup, binding, real)
	if err != nil {
		return "", errTurnStateMapping
	}
	s.issued[key] = record
	s.events = append(s.events, responseIdentityEvent{Action: "issued", Alias: record.Alias, Original: real, AccountID: account.ID(), Generation: generation})
	return record.Alias, nil
}
