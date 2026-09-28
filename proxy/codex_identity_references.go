package proxy

import (
	"context"
	"net/http"
	"sort"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexParentReferenceFields = []string{
	"parent_thread_id", "forked_from_thread_id", "guardian_classifier_source_thread_id",
	"x-codex-parent-thread-id", "x_codex_parent_thread_id", "x-codex-forked-from-thread-id", "x_codex_forked_from_thread_id",
}

var codexParentReferenceHeaders = []string{codexParentThreadIDHeader, "X-Codex-Forked-From-Thread-Id"}

func codexIdentityMetadataSources(headers http.Header, body []byte) []gjson.Result {
	metadata := gjson.GetBytes(body, "client_metadata")
	return []gjson.Result{metadata, diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")),
		diagnosticMetadataObject(metadata.Get("x_codex_turn_metadata")), gjson.Parse(headers.Get(codexTurnMetadataHeader))}
}

type codexParentReference struct {
	key                                           string
	epoch                                         database.CodexIdentityEpoch
	found, bound, legacyVerified, requiresMapping bool
	sourceKey                                     string
	source                                        database.SessionContinuityRecord
	sourceFound                                   bool
	reason                                        string
}

// Resolve once per request. The policy selection and alias mapping must use the
// same parent snapshot, rather than querying it twice around a concurrent switch.
func resolveCodexParentReference(ctx context.Context, store CodexIdentityStore, owner, account, rootKey, original string, accountID int64) (codexParentReference, error) {
	ref := codexParentReference{key: codexIdentityDigest("codex-account-reference-v1", rootKey, original)}
	var err error
	ref.epoch, ref.found, ref.bound, err = store.ReadCodexIdentityReference(ctx, ref.key, codexIdentityDigest("codex-account-root-v1", owner, account, original))
	if err != nil {
		ref.reason = "reference_lookup_failed"
		return ref, codexAccountIdentityError("暂时无法核实父会话出站身份，请稍后重试。")
	}
	ref.requiresMapping = ref.found
	if ref.bound {
		ref.legacyVerified = ref.epoch.RootKey != ""
		if ref.epoch.Detached {
			ref.reason = "previously_detached"
		}
		return ref, nil
	}
	ref.sourceKey, ref.source, ref.sourceFound, err = lookupCodexReferenceRoot(ctx, original)
	if err != nil {
		ref.reason = "parent_lookup_failed"
		return ref, codexAccountIdentityError("暂时无法核实父会话绑定账号，请稍后重试。")
	}
	ref.requiresMapping = ref.requiresMapping || ref.sourceFound && ref.source.AccountID == accountID && ref.source.OutboundWindowReset
	ref.legacyVerified = ref.sourceFound && ref.source.AccountID == accountID && ref.source.FailoverCount == 0 && !ref.source.OutboundWindowReset
	if !ref.found && ref.sourceFound && ref.source.AccountID != accountID {
		ref.reason = "parent_account_mismatch"
	} else if !ref.found && !ref.sourceFound {
		ref.reason = "parent_owner_unavailable"
	}
	return ref, nil
}

func (ref codexParentReference) resolvedEpoch(current database.CodexIdentityEpoch, accountID int64) database.CodexIdentityEpoch {
	resolved := ref.epoch
	if ref.bound {
		return resolved
	}
	if ref.found && resolved.RootKey != "" && resolved.RootKey == current.RootKey && resolved.Generation < current.Generation {
		resolved = current
	}
	if ref.sourceFound && ref.source.AccountID == accountID && (!ref.found || ref.source.FailoverCount > resolved.Generation) {
		epoch := &sessionOutboundEpoch{key: ref.sourceKey, record: ref.source}
		resolved = database.CodexIdentityEpoch{RootKey: ref.sourceKey, Generation: ref.source.FailoverCount, Segment: epoch.identityKey()}
	}
	return resolved
}

func sortedCodexParentReferences(headers http.Header, body []byte) []string {
	refs := codexAccountIdentityReferences(headers, body)
	values := make([]string, 0, len(refs))
	for value := range refs {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

type codexReferenceRootLookupKey struct{}
type codexReferenceRootLookup func(context.Context, string) (string, database.SessionContinuityRecord, bool, error)

func (handler *Handler) bindCodexReferenceRootLookup(request *gin.Context) {
	apiKeyID := requestAPIKeyID(request)
	status, policy := handler.cachedNewAPIPolicyAuditState(request)
	verified := (status == "verified" || status == "signed_response") && policy.MetaVerified && policy.Identity.UserID != ""
	lookup := codexReferenceRootLookup(func(ctx context.Context, original string) (string, database.SessionContinuityRecord, bool, error) {
		source := original
		if verified {
			source = "newapi-root-session:" + newAPIRootSessionFingerprint(policy.Platform, policy.Identity.UserID, original)
		}
		key := hashRiskIdentity(sessionAffinityKey(source, apiKeyID))
		entry, found, err := handler.readSessionContinuity(ctx, key)
		return key, entry.Record, found, err
	})
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), codexReferenceRootLookupKey{}, lookup))
}

func lookupCodexReferenceRoot(ctx context.Context, original string) (string, database.SessionContinuityRecord, bool, error) {
	if lookup, ok := ctx.Value(codexReferenceRootLookupKey{}).(codexReferenceRootLookup); ok {
		return lookup(ctx, original)
	}
	return "", database.SessionContinuityRecord{}, false, nil
}

func codexAccountIdentityReferences(headers http.Header, body []byte) map[string]bool {
	references := make(map[string]bool)
	add := func(value string) {
		if value = canonicalCodexAccountIdentity(value); value != "" {
			references[value] = true
		}
	}
	for _, name := range codexParentReferenceHeaders {
		add(headers.Get(name))
	}
	for _, source := range codexIdentityMetadataSources(headers, body) {
		for _, field := range codexParentReferenceFields {
			if value := source.Get(field); value.Type == gjson.String {
				add(value.String())
			}
		}
	}
	for _, name := range []string{codexSessionIDHeader, codexLegacySessionIDHeader, codexThreadIDHeader} {
		delete(references, canonicalCodexAccountIdentity(headers.Get(name)))
	}
	return references
}

// Shared by fork fallback, temporary background fallback and migrated roots.
// Only metadata is changed; input, tools and local signed lineage are untouched.
func detachCodexParentMetadata(raw string, detached map[string]bool) (string, error) {
	if len(detached) == 0 || !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return raw, nil
	}
	for _, field := range codexParentReferenceFields {
		if value := gjson.Get(raw, field); value.Type == gjson.String && detached[canonicalCodexAccountIdentity(value.String())] {
			var err error
			raw, err = sjson.Delete(raw, field)
			if err != nil {
				return "", err
			}
		}
	}
	return raw, nil
}

// Detach only the old parent references on the outbound envelope, before
// account identity mapping. Local signed identity, lineage and input are kept.
// Persistence is necessary because clients resend the parent on later turns.
func detachForkParentOutbound(body []byte, headers http.Header, references []string) ([]byte, http.Header, error) {
	if len(references) == 0 {
		return body, headers, nil
	}
	old := make(map[string]bool, len(references))
	for _, value := range references {
		old[canonicalCodexAccountIdentity(value)] = true
	}
	clean := func(raw string) (string, error) { return detachCodexParentMetadata(raw, old) }
	headers = headers.Clone()
	for _, name := range codexParentReferenceHeaders {
		if old[canonicalCodexAccountIdentity(headers.Get(name))] {
			headers.Del(name)
		}
	}
	if metadata := headers.Get(codexTurnMetadataHeader); metadata != "" {
		value, err := clean(metadata)
		if err != nil {
			return nil, nil, err
		}
		headers.Set(codexTurnMetadataHeader, value)
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if metadata.IsObject() {
		value, err := clean(metadata.Raw)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range []string{"x-codex-turn-metadata", "x_codex_turn_metadata"} {
			nested := gjson.Get(value, name)
			if nested.IsObject() || nested.Type == gjson.String {
				raw := nested.Raw
				if nested.Type == gjson.String {
					raw = nested.String()
				}
				cleaned, err := clean(raw)
				if err != nil {
					return nil, nil, err
				}
				if nested.Type == gjson.String {
					value, err = sjson.Set(value, name, cleaned)
				} else {
					value, err = sjson.SetRaw(value, name, cleaned)
				}
				if err != nil {
					return nil, nil, err
				}
			}
		}
		body, err = sjson.SetRawBytes(body, "client_metadata", []byte(value))
		if err != nil {
			return nil, nil, err
		}
	}
	return body, headers, nil
}
