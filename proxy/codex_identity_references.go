package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

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
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			references[value] = true
		}
	}
	for _, name := range []string{codexParentThreadIDHeader, "X-Codex-Forked-From-Thread-Id"} {
		add(headers.Get(name))
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	for _, source := range []gjson.Result{metadata, diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")), gjson.Parse(headers.Get(codexTurnMetadataHeader))} {
		for _, field := range []string{"guardian_classifier_source_thread_id", "parent_thread_id", "forked_from_thread_id", "x-codex-parent-thread-id", "x_codex_parent_thread_id", "x-codex-forked-from-thread-id", "x_codex_forked_from_thread_id"} {
			add(source.Get(field).String())
		}
	}
	for _, name := range []string{codexSessionIDHeader, codexLegacySessionIDHeader, codexThreadIDHeader} {
		delete(references, strings.ToLower(strings.TrimSpace(headers.Get(name))))
	}
	return references
}
