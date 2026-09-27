package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

type failoverDiagnosticStore struct {
	CodexIdentityStore
	failMapping bool
}

func (s *failoverDiagnosticStore) ResolveCodexIdentityMapping(ctx context.Context, key string, legacy []string, create bool) (database.CodexIdentityMappingPolicy, error) {
	if s.failMapping {
		return database.CodexIdentityMappingPolicy{}, errors.New("private-db-secret")
	}
	return s.CodexIdentityStore.ResolveCodexIdentityMapping(ctx, key, legacy, create)
}
func (s *failoverDiagnosticStore) ClaimCodexIdentities(context.Context, []string, string) error {
	return errors.New("private-db-secret")
}

func TestSessionFailoverIdentityFailureDetails(t *testing.T) {
	for _, mapping := range []bool{false, true} {
		t.Run(map[bool]string{false: "claim", true: "mapping"}[mapping], func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, true)
			owner.Status = auth.StatusError
			c, body := failoverTestRequest(t, h)
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			ctx := WithCodexIdentityStore(c.Request.Context(), &failoverDiagnosticStore{CodexIdentityStore: h.db, failMapping: mapping})
			selected, _, handled := h.takeSessionAccountFailover(ctx, key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(t, handled)
			require.Nil(t, selected)
			selection := usageRequestDiagnosticState(c).AccountFailover.Selection
			var detail *database.SessionFailoverIdentityFailure
			for _, candidate := range selection.Candidates {
				if candidate.AccountID == target.ID() && candidate.Reason == "outbound_identity_unavailable" {
					detail = candidate.IdentityFailure
				}
			}
			require.NotNil(t, detail)
			require.Equal(t, map[bool]string{false: "identity_claim", true: "mapping_policy"}[mapping], detail.Stage)
			require.Equal(t, "codex_session_identity_unavailable", detail.Code)
			require.Equal(t, 400, detail.HTTPStatus)
			encoded, err := json.Marshal(selection)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "private-db-secret")
			require.NotContains(t, string(encoded), continuityTestThread)
			record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, owner.ID(), record.AccountID)
			require.Zero(t, target.ActiveRequests)
			// Verify the detail survives the persisted service-error path too.
			finishAudit := h.beginServiceErrorAudit(c)
			serviceErrorAuditForRequest(c).authenticated = true
			h.sendDispatchUnavailable(c, false, false)
			finishAudit()
			page := serviceErrorTestPage(t, h)
			require.Len(t, page.Items, 1)
			require.NotNil(t, page.Items[0].AccountFailover)
			var stored *database.SessionFailoverIdentityFailure
			for _, candidate := range page.Items[0].AccountFailover.Selection.Candidates {
				if candidate.AccountID == target.ID() && candidate.Reason == "outbound_identity_unavailable" {
					stored = candidate.IdentityFailure
				}
			}
			require.Equal(t, detail, stored)
		})
	}
}
