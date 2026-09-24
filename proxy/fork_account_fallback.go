package proxy

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type forkAccountFallbackKey struct{}

type forkAccountFallbackDiagnostic struct {
	Reason          string `json:"reason"`
	ParentAccountID int64  `json:"parent_account_id,omitempty"`
	PreserveInput   bool   `json:"preserve_input"`
	references      []string
}

func forkAccountFallbackFromContext(ctx context.Context) *forkAccountFallbackDiagnostic {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(forkAccountFallbackKey{}).(*forkAccountFallbackDiagnostic)
	return value
}

// This is initial account selection for a separate user root, never permission
// for a background task or an already-bound conversation to migrate.
func allowForkAccountFallback(request *gin.Context, identity requestSessionIdentity) bool {
	if !CurrentRuntimeSettings().CodexForkAccountFallbackEnabled || !identity.stableIdentity || identity.forkSourceAffinityID == "" || identity.requiresRootAccount || identity.bypassWindowAccounting || identity.unlinkedFallbackOnly {
		return false
	}
	resolved := usageRequestDiagnosticState(request).Resolved
	return resolved != nil && strings.EqualFold(resolved.ThreadSource, "user") && resolved.SubagentKind == "" && (resolved.RequestKind == "" || strings.EqualFold(resolved.RequestKind, "turn"))
}

func (handler *Handler) forkAccountFallbackReason(request *gin.Context, parentID int64, childKey string) string {
	parent := handler.store.FindByID(parentID)
	if parentID == 0 || parent == nil {
		return "fork_parent_missing"
	}
	if !parent.IsRelayStyle() && parent.SessionCapacityLimits().Enabled && !handler.store.CanAdmitAccountSession(parent, childKey, time.Now(), selectionTraceForRequest(request)) {
		return "fork_parent_capacity_full"
	}
	return ""
}

func beginForkAccountFallback(request *gin.Context, body []byte, diagnostic *sessionContinuityDiagnostic, parentID int64, reason string) {
	fallback := &forkAccountFallbackDiagnostic{Reason: reason, ParentAccountID: parentID, PreserveInput: CurrentRuntimeSettings().CodexSessionFailoverPreserveInput}
	for reference := range codexAccountIdentityReferences(sessionFailoverRequestHeaders(request), body) {
		fallback.references = append(fallback.references, reference)
	}
	sort.Strings(fallback.references)
	diagnostic.ForkAccountFallback = fallback
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), forkAccountFallbackKey{}, fallback))
}

// New fork quotes leave the owner open, including when the parent currently has
// space. Capacity can change before dispatch; a quote must not hard-bind a fork
// to the account it is only meant to prefer. Existing child bindings/grants are
// resolved before this function and remain authoritative.
func (handler *Handler) deferForkQuoteOwner(request *gin.Context, parentID int64, childKey string) {
	diagnostic := windowControlDiagnostic(request)
	if diagnostic == nil {
		return
	}
	diagnostic.OwnerSource = "fork_parent_preferred"
	if parent := handler.store.FindByID(parentID); parent != nil {
		_, capacity := handler.store.CanAdmitAccountSessionWithDiagnostic(parent, childKey, time.Now())
		diagnostic.Account = &capacity
	} else {
		diagnostic.OwnerSource = "fork_parent_missing"
	}
}

func forkFallbackAccountFilter(ctx context.Context, account *auth.Account) bool {
	if forkAccountFallbackFromContext(ctx) == nil {
		return true
	}
	// A fork restart has Codex/BPS identity and context semantics. Do not commit
	// this new binding to a relay with a different protocol or authorization domain.
	return account != nil && !account.IsRelayStyle()
}

func forkFallbackRestartRecord(next *database.SessionContinuityRecord, request *gin.Context) {
	if fallback := forkAccountFallbackFromContext(request.Request.Context()); fallback != nil {
		next.PreserveRestartInput = fallback.PreserveInput
		next.DetachedForkReferences = append([]string(nil), fallback.references...)
	}
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
		old[strings.ToLower(strings.TrimSpace(value))] = true
	}
	fields := []string{"parent_thread_id", "forked_from_thread_id", "guardian_classifier_source_thread_id", "x-codex-parent-thread-id", "x_codex_parent_thread_id", "x-codex-forked-from-thread-id", "x_codex_forked_from_thread_id"}
	clean := func(raw string) (string, error) {
		if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
			return raw, nil
		}
		var err error
		for _, field := range fields {
			if value := gjson.Get(raw, field); value.Type == gjson.String && old[strings.ToLower(strings.TrimSpace(value.String()))] {
				raw, err = sjson.Delete(raw, field)
				if err != nil {
					return "", err
				}
			}
		}
		return raw, nil
	}
	headers = headers.Clone()
	for _, name := range []string{codexParentThreadIDHeader, "X-Codex-Forked-From-Thread-Id"} {
		if old[strings.ToLower(strings.TrimSpace(headers.Get(name)))] {
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
