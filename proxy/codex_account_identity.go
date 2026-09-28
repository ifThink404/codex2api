package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type CodexIdentityStore interface {
	ClaimCodexIdentities(context.Context, []string, string) error
	ResolveCodexIdentityMapping(context.Context, string, []string, bool) (database.CodexIdentityMappingPolicy, error)
	ResolveCodexIdentityUUIDv7(context.Context, string, string) (string, error)
	ClaimCodexIdentityAliases(context.Context, []database.CodexIdentityAliasClaim) error
	PublishCodexIdentityEpoch(context.Context, string, database.CodexIdentityEpoch) error
	ReadCodexIdentityReference(context.Context, string, string) (database.CodexIdentityEpoch, bool, bool, error)
	ClaimCodexIdentityReference(context.Context, string, database.CodexIdentityEpoch) error
}

type codexAnonymousIdentityContextKey struct{}

func WithCodexIdentityStore(ctx context.Context, store CodexIdentityStore) context.Context {
	if ctx.Value(codexAnonymousIdentityContextKey{}) == nil {
		ctx = context.WithValue(ctx, codexAnonymousIdentityContextKey{}, "anonymous:"+NewUpstreamSessionUUID())
	}
	return context.WithValue(ctx, codexIdentityClaimerContextKey{}, store)
}

type codexAccountIdentityChange struct {
	Original string     `json:"original"`
	Outbound string     `json:"outbound"`
	Fields   []string   `json:"fields,omitempty"`
	Version  string     `json:"version,omitempty"`
	MappedAt *time.Time `json:"mapped_at,omitempty"`
}

type codexAccountIdentityDiagnosticKey struct{}

type codexAccountReferenceDiagnostic struct {
	Original    string `json:"original"`
	Policy      string `json:"policy"`
	Generation  uint64 `json:"generation"`
	SegmentHash string `json:"segment_hash,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type codexAccountIdentityDiagnostic struct {
	FailureStage        string                              `json:"failure_stage,omitempty"`
	Version             string                              `json:"version,omitempty"`
	Status              string                              `json:"status"`
	FallbackSource      string                              `json:"fallback_source,omitempty"`
	ScopeHash           string                              `json:"scope_hash,omitempty"`
	UpstreamAccount     string                              `json:"chatgpt_account_id,omitempty"`
	CachePartitioned    bool                                `json:"cache_partitioned,omitempty"`
	Changes             []codexAccountIdentityChange        `json:"changes,omitempty"`
	PreservedIDs        []string                            `json:"preserved_ids,omitempty"`
	Generation          uint64                              `json:"generation,omitempty"`
	SegmentHash         string                              `json:"segment_hash,omitempty"`
	Windows             []codexAccountWindowChange          `json:"windows,omitempty"`
	References          []codexAccountReferenceDiagnostic   `json:"references,omitempty"`
	InvalidTurnIdentity *codexInvalidTurnIdentityDiagnostic `json:"invalid_turn_identity,omitempty"`
}

type codexInvalidTurnIdentityDiagnostic struct {
	Stage       string   `json:"stage"`
	Sources     []string `json:"sources"`
	Reason      string   `json:"reason"`
	Expected    string   `json:"expected"`
	UUIDVersion int      `json:"uuid_version,omitempty"`
	UUIDVariant string   `json:"uuid_variant,omitempty"`
	ValueHash   string   `json:"value_hash"`
	ValueLength int      `json:"value_length"`
}

type codexAccountIdentity struct {
	mode               string
	secret             []byte
	owner              string
	account            string
	epoch              string
	preserveRoot       bool
	windowNumbers      map[string]uint64
	detachedReferences map[string]bool
	aliases            map[string]string
	turnAliases        map[string]string
	requestAliases     map[string]string
	diagnostic         codexAccountIdentityDiagnostic
	protocolDB         *database.DB
	protocolBinding    database.CodexTurnStateBinding
}

var codexAccountIdentityFields = append([]string{
	"session_id", "thread_id", "context_window_id",
	"x-codex-context-window-id", "x_codex_context_window_id",
}, codexParentReferenceFields...)

var codexAccountMetadataFields = append(append([]string(nil), codexAccountIdentityFields...),
	"x-client-request-id", "client_request_id", "x_client_request_id", "window_id", "x-codex-window-id", "x_codex_window_id")

func codexAccountIdentityInputs(headers http.Header, body []byte) []string {
	values := codexTransportIdentityValues(headers, body)
	for _, source := range codexIdentityMetadataSources(headers, body) {
		for _, field := range codexAccountIdentityFields {
			if value := source.Get(field); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				values = append(values, value.String())
			}
		}
		for _, field := range []string{"window_id", "x-codex-window-id", "x_codex_window_id"} {
			if value := source.Get(field); value.Type == gjson.String {
				if separator := strings.LastIndexByte(value.String(), ':'); separator > 0 {
					values = append(values, value.String()[:separator])
				}
			}
		}
	}
	return values
}

func (fingerprint *CodexFingerprint) prepareAccountIdentity(ctx context.Context, account *auth.Account, owner string, accountScopes []string) (requestErr error) {
	stage := "root_identity"
	diagnostic := codexAccountIdentityDiagnostic{Status: "not_applicable"}
	if fallback := fingerprint.sessionIdentityFallback; fallback != nil {
		diagnostic.FallbackSource = fallback.source
	}
	defer func() {
		if requestErr != nil {
			diagnostic.FailureStage = stage
		}
		fingerprint.accountIdentityDiagnostic = &diagnostic
		UpstreamTransportObserver(ctx).updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
			identity.AccountMapping = &diagnostic
		})
	}()
	root := strings.TrimSpace(fingerprint.headers.Get(codexSessionIDHeader))
	if root == "" {
		root = strings.TrimSpace(fingerprint.headers.Get(codexLegacySessionIDHeader))
	}
	if root == "" {
		if fingerprint.accountIdentityRequested && len(fingerprint.identityValues) > 0 {
			diagnostic.Status = "missing_root"
			return codexAccountIdentityError("缺少明确的根会话标识，无法生成账号级出站身份。")
		}
		return nil
	}
	stage = "identity_store"
	store, available := ctx.Value(codexIdentityClaimerContextKey{}).(CodexIdentityStore)
	if !available {
		if fingerprint.accountIdentityRequested {
			diagnostic.Status = "store_unavailable"
			return codexAccountIdentityError("账号级出站身份需要持久化存储，当前无法使用。")
		}
		return nil
	}
	diagnostic.Status = "failed"
	stage = "upstream_account"
	upstreamAccount := strings.TrimSpace(account.EffectiveAccountID())
	if upstreamAccount == "" {
		if fingerprint.accountIdentityRequested {
			return codexAccountIdentityError("缺少实际 Chatgpt-Account-Id，无法生成账号级出站身份。")
		}
		diagnostic.Status = "not_applicable"
		return nil
	}
	rootKey := codexIdentityDigest("codex-account-root-v1", owner, upstreamAccount, canonicalCodexAccountIdentity(root))
	epoch := outboundEpochFromContext(ctx)
	epochKey := epoch.identityKey()
	legacyKeys := make([]string, 0, len(accountScopes))
	for _, scope := range accountScopes {
		legacyKeys = append(legacyKeys, codexIdentityDigest("codex-session-v1", scope, root))
	}
	if epochKey != "" {
		rootKey = codexIdentityDigest("codex-account-segment-root-v1", owner, upstreamAccount, epochKey, canonicalCodexAccountIdentity(root))
		legacyKeys = nil
		diagnostic.Generation, diagnostic.SegmentHash = epoch.record.FailoverCount, epochKey[:24]
	}
	stage = "parent_reference"
	mappedReference := false
	parentReferences := make(map[string]codexParentReference, len(fingerprint.accountIdentityReferences))
	parentOrder := make([]string, 0, len(fingerprint.accountIdentityReferences))
	for original := range fingerprint.accountIdentityReferences {
		parentOrder = append(parentOrder, original)
	}
	sort.Strings(parentOrder)
	for _, original := range parentOrder {
		ref, err := resolveCodexParentReference(ctx, store, owner, upstreamAccount, rootKey, original, account.ID())
		if err != nil {
			diagnostic.References = append(diagnostic.References, codexAccountReferenceDiagnostic{Original: original, Action: "blocked", Reason: ref.reason})
			return err
		}
		parentReferences[original] = ref
		mappedReference = mappedReference || ref.requiresMapping
	}
	allowDetach := currentCodexSessionRecoveryPolicy().detachUnavailableParent(epoch, account.ID())
	stage = "mapping_policy"
	policy, err := store.ResolveCodexIdentityMapping(ctx, rootKey, legacyKeys, fingerprint.accountIdentityRequested || epochKey != "" || mappedReference)
	if err == nil && policy.Mode == "preserve" && fingerprint.isolateConflictingIdentity {
		// Leave the legacy owner's policy intact. Reuse the global mapping
		// secret but create an isolated policy for this authenticated owner.
		policy, err = store.ResolveCodexIdentityMapping(ctx, codexIdentityDigest("codex-owner-isolated-policy-v1", rootKey), nil, true)
	}
	if err != nil {
		return codexAccountIdentityError("暂时无法核实出站身份映射，请稍后重试。")
	}
	diagnostic.UpstreamAccount = diagnosticIdentifier(upstreamAccount)
	diagnostic.ScopeHash = rootKey[:24]
	diagnostic.Version = policy.Mode
	preserveRoot := policy.Mode == "preserve"
	if preserveRoot && codexIdentityEpochMigrated(epoch) {
		return codexAccountIdentityError("已换号的会话不能恢复原始出站身份，已停止发送，请核实迁移记录。")
	}
	if preserveRoot && (len(fingerprint.accountIdentityReferences) == 0 || !fingerprint.accountIdentityRequested && !mappedReference) {
		diagnostic.Status = "preserved_existing"
		return nil
	}
	if !preserveRoot && policy.Mode != "account-suffix-v1" && policy.Mode != database.CodexIdentityMappingUUIDv7 {
		return codexAccountIdentityError("出站身份映射版本不受支持，请检查服务版本。")
	}
	secret, err := hex.DecodeString(policy.Secret)
	if !preserveRoot && (err != nil || len(secret) != 32) {
		return codexAccountIdentityError("出站身份映射密钥不可用，请恢复完整数据库。")
	}
	mapping := &codexAccountIdentity{mode: policy.Mode, secret: secret, owner: owner, account: upstreamAccount, epoch: epochKey, preserveRoot: preserveRoot, aliases: make(map[string]string), detachedReferences: make(map[string]bool)}
	mapping.protocolDB, mapping.protocolBinding = protocolIdentityBinding(ctx, account)
	stage = "window_mapping"
	if err := fingerprint.prepareAccountWindows(ctx, mapping, epoch); err != nil {
		return err
	}
	diagnostic.Windows = mapping.diagnostic.Windows
	sort.Slice(diagnostic.Windows, func(left, right int) bool {
		return diagnostic.Windows[left].ThreadID < diagnostic.Windows[right].ThreadID
	})
	stage = "identity_input"
	values := make(map[string]bool)
	for _, original := range fingerprint.accountIdentityInputs {
		if original = strings.TrimSpace(original); original == "" {
			continue
		}
		if len(original) > 512 {
			return codexAccountIdentityError("会话标识过长，请检查客户端请求。")
		}
		values[canonicalCodexAccountIdentity(original)] = true
	}
	if len(values) == 0 || len(values) > 32 {
		return codexAccountIdentityError("出站会话身份数量无效，请检查客户端元数据。")
	}
	ordered := make([]string, 0, len(values))
	for original := range values {
		ordered = append(ordered, original)
	}
	sort.Strings(ordered)
	claims := make([]database.CodexIdentityAliasClaim, 0, len(ordered))
	references := make(map[string]database.CodexIdentityEpoch)
	legacyParentPreserved := false
	currentEpoch := database.CodexIdentityEpoch{Segment: epochKey}
	if policy.Mode == database.CodexIdentityMappingUUIDv7 {
		currentEpoch.MappingVersion = policy.Mode
	}
	if epoch != nil {
		currentEpoch.RootKey, currentEpoch.Generation = epoch.key, epoch.record.FailoverCount
	}
	stage = "identity_mapping"
	for _, original := range ordered {
		if preserveRoot && !fingerprint.accountIdentityReferences[original] {
			diagnostic.PreservedIDs = append(diagnostic.PreservedIDs, original)
			continue
		}
		baseIdentityKey := codexIdentityDigest("codex-account-root-v1", owner, upstreamAccount, original)
		identityKey := baseIdentityKey
		identityEpoch := currentEpoch
		legacyParentVerified := false
		var referenceDiagnostic *codexAccountReferenceDiagnostic
		var parentReference codexParentReference
		if fingerprint.accountIdentityReferences[original] {
			parentReference = parentReferences[original]
			diagnostic.References = append(diagnostic.References, codexAccountReferenceDiagnostic{Original: original, Action: "blocked"})
			referenceDiagnostic = &diagnostic.References[len(diagnostic.References)-1]
			identityEpoch = parentReference.resolvedEpoch(currentEpoch, account.ID())
			legacyParentVerified = parentReference.legacyVerified
			if identityEpoch.Detached || allowDetach && parentReference.reason != "" {
				mapping.detachedReferences[original] = true
				references[parentReference.key] = database.CodexIdentityEpoch{Detached: true}
				referenceDiagnostic.Action, referenceDiagnostic.Reason = "detached", parentReference.reason
				continue
			}
			if parentReference.reason == "parent_account_mismatch" {
				referenceDiagnostic.Reason = parentReference.reason
				return codexAccountIdentityError("无法核实该账号对应的父会话出站段，已停止发送父引用。")
			}
		}
		identityLegacyKeys := make([]string, 0, len(accountScopes))
		for _, scope := range accountScopes {
			identityLegacyKeys = append(identityLegacyKeys, codexIdentityDigest("codex-session-v1", scope, original))
		}
		if identityEpoch.Segment != "" {
			identityKey = codexIdentityDigest("codex-account-segment-root-v1", owner, upstreamAccount, identityEpoch.Segment, original)
			identityLegacyKeys = nil
		}
		identityPolicy, err := store.ResolveCodexIdentityMapping(ctx, identityKey, identityLegacyKeys, true)
		if err == nil && identityPolicy.Mode == "preserve" && fingerprint.isolateConflictingIdentity {
			identityPolicy, err = store.ResolveCodexIdentityMapping(ctx, codexIdentityDigest("codex-owner-isolated-policy-v1", identityKey), nil, true)
		}
		if err != nil {
			return codexAccountIdentityError("暂时无法核实关联会话身份映射，请稍后重试。")
		}
		if referenceDiagnostic != nil {
			referenceDiagnostic.Policy, referenceDiagnostic.Generation = identityPolicy.Mode, identityEpoch.Generation
			if identityEpoch.Segment != "" {
				referenceDiagnostic.SegmentHash = diagnosticIdentifier(identityEpoch.Segment)
			}
		}
		if identityPolicy.Mode == "preserve" {
			if referenceDiagnostic != nil {
				referenceDiagnostic.Reason = legacyCodexParentReferenceBlock(epoch, account.ID(), identityEpoch, legacyParentVerified)
				if referenceDiagnostic.Reason != "" {
					if allowDetach && !parentReference.bound {
						mapping.detachedReferences[original] = true
						references[parentReference.key] = database.CodexIdentityEpoch{Detached: true}
						referenceDiagnostic.Action = "detached"
						continue
					}
					return codexAccountIdentityError("父会话尚无可确认的账号级出站映射，且不符合原账号旧会话兼容条件，已停止发送原始父会话 ID。")
				}
				referenceDiagnostic.Action, referenceDiagnostic.Reason = "preserved_legacy_parent", "original_account_unmigrated"
				legacyParentPreserved = true
				references[parentReference.key] = identityEpoch
			} else if codexIdentityEpochMigrated(epoch) {
				return codexAccountIdentityError("已换号的会话不能发送旧的原始关联身份，已停止发送，请核实迁移记录。")
			}
			diagnostic.PreservedIDs = append(diagnostic.PreservedIDs, original)
			continue
		}
		if identityPolicy.Mode != "account-suffix-v1" && identityPolicy.Mode != database.CodexIdentityMappingUUIDv7 || len(mapping.secret) > 0 && identityPolicy.Secret != hex.EncodeToString(mapping.secret) {
			return codexAccountIdentityError("关联会话身份映射不一致，请检查数据库完整性。")
		}
		if len(mapping.secret) == 0 {
			mapping.secret, err = hex.DecodeString(identityPolicy.Secret)
			if err != nil || len(mapping.secret) != 32 {
				return codexAccountIdentityError("出站身份映射密钥不可用，请恢复完整数据库。")
			}
		}
		identityMapping := *mapping
		identityMapping.epoch = identityEpoch.Segment
		identityMapping.mode = identityPolicy.Mode
		if parsed, err := uuid.Parse(original); err != nil || parsed.Version() != 7 || parsed.Variant() != uuid.RFC4122 {
			// sever accepts SDK UUIDv4 and opaque session/context identifiers.
			// Allocate a stable account-scoped UUIDv7 instead of forwarding them
			// unchanged or using the legacy UUID suffix transformation.
			identityMapping.mode = database.CodexIdentityMappingUUIDv7
		}
		outbound, err := identityMapping.mapUUID(ctx, store, "identity", original)
		if err != nil {
			return err
		}
		mapping.aliases[original] = outbound
		if referenceDiagnostic != nil {
			referenceDiagnostic.Action = "mapped"
			references[parentReference.key] = identityEpoch
		}
		diagnostic.Changes = append(diagnostic.Changes, identityMapping.identityChange(original, outbound))
		sourceKey := codexIdentityDigest("codex-account-alias-source-v1", owner, upstreamAccount, original)
		if identityEpoch.Segment != "" {
			sourceKey = codexIdentityDigest("codex-account-segment-source-v1", owner, upstreamAccount, identityEpoch.Segment, original)
		}
		claims = append(claims, database.CodexIdentityAliasClaim{
			AliasKey:  codexIdentityDigest("codex-account-alias-v1", outbound),
			SourceKey: sourceKey,
		})
	}
	stage = "turn_mapping"
	turnPlan, err := fingerprint.prepareAccountTurnIdentity(ctx, store, mapping, rootKey, currentEpoch, &diagnostic)
	if err != nil {
		return err
	}
	claims = append(claims, turnPlan.claims...)
	stage = "alias_registration"
	for start := 0; start < len(claims); start += 32 {
		if err := store.ClaimCodexIdentityAliases(ctx, claims[start:min(start+32, len(claims))]); err != nil {
			if errors.Is(err, database.ErrCodexIdentityAliasCollision) {
				return codexAccountIdentityError("出站会话标识发生冲突，已停止请求，请联系管理员。")
			}
			return codexAccountIdentityError("暂时无法登记出站身份映射，请稍后重试。")
		}
	}
	stage = "epoch_registration"
	if epoch == nil || !epoch.preview {
		if err := ValidateBackgroundAccountMatch(ctx, account); err != nil {
			return err
		}
		if mapping.protocolDB != nil {
			for original, outbound := range mapping.turnAliases {
				if original == outbound {
					continue
				}
				if err := mapping.protocolDB.PutCodexProtocolPair(ctx, mapping.protocolBinding, "turn", database.CodexProtocolPair{Public: original, Upstream: outbound}); err != nil {
					return codexAccountIdentityError("轮次双向映射冲突或不可用，已停止发送，请重试。")
				}
			}
		}
		if epoch == nil || !epoch.temporary {
			for identityKey, turnEpoch := range turnPlan.epochs {
				if err := store.PublishCodexIdentityEpoch(ctx, identityKey, turnEpoch); err != nil {
					return codexAccountIdentityError("轮次映射阶段已变化或暂时不可用，已停止发送，请重试。")
				}
			}
			for referenceKey, turnEpoch := range turnPlan.references {
				if err := store.ClaimCodexIdentityReference(ctx, referenceKey, turnEpoch); err != nil {
					return codexAccountIdentityError("轮次引用映射已变化或暂时不可用，已停止发送，请重试。")
				}
			}
			for referenceKey, referenceEpoch := range references {
				if err := store.ClaimCodexIdentityReference(ctx, referenceKey, referenceEpoch); err != nil {
					return codexAccountIdentityError("父会话出站引用已变化或暂时不可用，请重新发起请求。")
				}
			}
			for _, original := range []string{canonicalCodexAccountIdentity(root), canonicalCodexAccountIdentity(fingerprint.headers.Get(codexThreadIDHeader))} {
				if mapping.aliases[original] == "" || fingerprint.accountIdentityReferences[original] {
					continue
				}
				if err := store.PublishCodexIdentityEpoch(ctx, codexIdentityDigest("codex-account-root-v1", owner, upstreamAccount, original), currentEpoch); err != nil {
					return codexAccountIdentityError("会话出站映射代数已变化或暂时不可用，请重新发起请求。")
				}
			}
		}
	}
	diagnostic.Version = policy.Mode
	diagnostic.Status = "mapped"
	if preserveRoot {
		diagnostic.Status = "preserved_with_mapped_references"
	}
	if legacyParentPreserved {
		diagnostic.Status = "mapped_with_legacy_references"
		if preserveRoot {
			diagnostic.Status = "preserved_with_legacy_references"
		}
	}
	mapping.diagnostic = diagnostic
	mapping.requestAliases = make(map[string]string)
	for _, original := range fingerprint.accountRequestIdentityInputs {
		if strings.TrimSpace(original) == "" {
			continue
		}
		mapped := mapping.rewriteValue(original)
		if mapped == original && len(mapping.secret) > 0 {
			mapped = DeriveStableSessionUUIDv7(mapping.digest("client-request", original))
		}
		mapping.requestAliases[original] = mapped
		if mapped != original && mapping.rewriteValue(original) == original {
			diagnostic.Changes = append(diagnostic.Changes, codexAccountIdentityChange{Original: original, Outbound: mapped, Fields: []string{"client_request_id"}, Version: "request-hmac-v1"})
		}
	}
	mapping.diagnostic = diagnostic
	fingerprint.accountIdentity = mapping
	fingerprint.headers = mapping.rewriteHeaders(fingerprint.headers)
	return nil
}

func codexAccountIdentityError(message string) *Error {
	return &Error{Code: "codex_session_identity_unavailable", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

func codexIdentityEpochMigrated(epoch *sessionOutboundEpoch) bool {
	return epoch != nil && (epoch.record.FailoverCount > 0 || epoch.record.OutboundWindowReset)
}

func legacyCodexParentReferenceBlock(epoch *sessionOutboundEpoch, accountID int64, parent database.CodexIdentityEpoch, verified bool) string {
	if epoch == nil || epoch.key == "" || epoch.record.AccountID != accountID {
		return "request_epoch_unavailable"
	}
	if codexIdentityEpochMigrated(epoch) {
		return "request_migrated"
	}
	if parent.Generation > 0 || parent.Segment != "" {
		return "parent_migrated"
	}
	if !verified || parent.RootKey == "" {
		return "parent_owner_unavailable"
	}
	return ""
}

func (mapping *codexAccountIdentity) digest(domain, original string) string {
	parts := []string{"account-suffix-v1", domain, mapping.owner, mapping.account, original}
	if mapping.mode == database.CodexIdentityMappingUUIDv7 {
		parts[0] = mapping.mode
	}
	if mapping.epoch != "" {
		parts = append(parts, mapping.epoch)
	}
	encoded, _ := json.Marshal(parts)
	mac := hmac.New(sha256.New, mapping.secret)
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

func canonicalCodexAccountIdentity(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := uuid.Parse(value); err == nil {
		return parsed.String()
	}
	return value
}

func (mapping *codexAccountIdentity) rewriteValue(original string) string {
	if alias := mapping.aliases[canonicalCodexAccountIdentity(original)]; alias != "" {
		return alias
	}
	return original
}

func (mapping *codexAccountIdentity) rewriteWindow(original string) string {
	if separator := strings.LastIndexByte(original, ':'); separator >= 0 {
		thread := strings.ToLower(original[:separator])
		suffix := original[separator:]
		if number, found := mapping.windowNumbers[thread]; found {
			suffix = ":" + strconv.FormatUint(number, 10)
		}
		return mapping.rewriteValue(original[:separator]) + suffix
	}
	return original
}

func (mapping *codexAccountIdentity) rewriteMetadata(raw string, fallbackThreads ...string) string {
	raw, _ = detachCodexParentMetadata(raw, mapping.detachedReferences)
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return raw
	}
	thread := gjson.Get(raw, "thread_id").String()
	if thread == "" {
		thread = gjson.Get(raw, "session_id").String()
	}
	for _, field := range []string{"window_id", "x-codex-window-id", "x_codex_window_id"} {
		if windowThread, _, err := parseAccountWindow(gjson.Get(raw, field).String()); err == nil {
			thread = windowThread
			break
		}
	}
	if thread == "" && len(fallbackThreads) > 0 {
		thread = fallbackThreads[0]
	}
	if number, found := mapping.windowNumbers[strings.ToLower(thread)]; found {
		if value := gjson.Get(raw, "window_number"); value.Type == gjson.Number {
			raw, _ = sjson.Set(raw, "window_number", number)
		}
	}
	for _, field := range codexAccountMetadataFields {
		value := gjson.Get(raw, field)
		if value.Type != gjson.String {
			continue
		}
		updated := mapping.rewriteValue(value.String())
		if field == "x-client-request-id" || field == "client_request_id" || field == "x_client_request_id" {
			updated = mapping.rewriteRequestValue(value.String())
		}
		if field == "window_id" || field == "x-codex-window-id" || field == "x_codex_window_id" {
			updated = mapping.rewriteWindow(value.String())
		}
		if updated != value.String() {
			raw, _ = sjson.Set(raw, field, updated)
		}
	}
	for _, field := range []string{"turn_id", "root_turn_id", "parent_turn_id"} {
		value := gjson.Get(raw, field)
		if value.Type == gjson.String {
			if alias := mapping.rewriteTurnValue(value.String()); alias != value.String() {
				raw, _ = sjson.Set(raw, field, alias)
			}
		}
	}
	return raw
}

func (mapping *codexAccountIdentity) rewriteHeaders(headers http.Header) http.Header {
	headers = headers.Clone()
	for _, name := range codexParentReferenceHeaders {
		if mapping.detachedReferences[canonicalCodexAccountIdentity(headers.Get(name))] {
			headers.Del(name)
		}
	}
	originalThread := headers.Get(codexThreadIDHeader)
	for _, name := range []string{codexSessionIDHeader, codexLegacySessionIDHeader, codexThreadIDHeader, codexClientRequestIDHeader, codexParentThreadIDHeader, "X-Codex-Forked-From-Thread-Id"} {
		if value := headers.Get(name); value != "" {
			if name == codexClientRequestIDHeader {
				headers.Set(name, mapping.rewriteRequestValue(value))
			} else {
				headers.Set(name, mapping.rewriteValue(value))
			}
		}
	}
	if window := headers.Get(codexWindowIDHeader); window != "" {
		headers.Set(codexWindowIDHeader, mapping.rewriteWindow(window))
	}
	if metadata := headers.Get(codexTurnMetadataHeader); metadata != "" {
		headers.Set(codexTurnMetadataHeader, mapping.rewriteMetadata(metadata, originalThread))
	}
	return headers
}

func codexAccountRequestIdentityInputs(headers http.Header, body []byte) []string {
	values := []string{headers.Get(codexClientRequestIDHeader)}
	for _, source := range codexIdentityMetadataSources(headers, body) {
		for _, field := range []string{"x-client-request-id", "client_request_id", "x_client_request_id"} {
			if value := source.Get(field); value.Type == gjson.String {
				values = append(values, value.String())
			}
		}
	}
	return values
}

func (mapping *codexAccountIdentity) rewriteRequestValue(original string) string {
	if alias := mapping.requestAliases[original]; alias != "" {
		return alias
	}
	return mapping.rewriteValue(original)
}

func (mapping *codexAccountIdentity) rewriteBody(body []byte) []byte {
	body = rewriteHistoryTurnIDs(body, mapping.rewriteTurnValue)
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return body
	}
	outerThread := accountMetadataThread(metadata)
	innerThread := accountMetadataThread(diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")))
	raw := mapping.rewriteMetadata(metadata.Raw, innerThread)
	for _, name := range []string{"x-codex-turn-metadata", "x_codex_turn_metadata"} {
		embedded := gjson.Get(raw, name)
		if embedded.Type == gjson.String {
			raw, _ = sjson.Set(raw, name, mapping.rewriteMetadata(embedded.String(), outerThread))
		} else if embedded.IsObject() {
			raw, _ = sjson.SetRaw(raw, name, mapping.rewriteMetadata(embedded.Raw, outerThread))
		}
	}
	updated, err := sjson.SetRawBytes(body, "client_metadata", []byte(raw))
	if err != nil {
		return body
	}
	return updated
}

func (fingerprint *CodexFingerprint) ScopeCacheKey(ctx context.Context, cacheKey string) string {
	if fingerprint.accountIdentity == nil || fingerprint.accountIdentity.preserveRoot || strings.TrimSpace(cacheKey) == "" {
		return cacheKey
	}
	diagnostic := fingerprint.accountIdentity.diagnostic
	diagnostic.CachePartitioned = true
	fingerprint.accountIdentityDiagnostic = &diagnostic
	UpstreamTransportObserver(ctx).updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.AccountMapping = &diagnostic
	})
	return fingerprint.accountIdentity.digest("prompt-cache", cacheKey)
}

func (fingerprint *CodexFingerprint) withAccountIdentityDiagnostic(ctx context.Context) context.Context {
	if fingerprint.accountIdentityDiagnostic == nil {
		return ctx
	}
	return context.WithValue(ctx, codexAccountIdentityDiagnosticKey{}, fingerprint.accountIdentityDiagnostic)
}

func (fingerprint *CodexFingerprint) WithAccountIdentityDiagnostic(ctx context.Context) context.Context {
	return fingerprint.withAccountIdentityDiagnostic(ctx)
}
