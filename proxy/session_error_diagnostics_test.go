package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWindowOwnerLookupDiagnostics(t *testing.T) {
	handler := newWindowAuthorizationHandler(t)
	parent, child := promptSessionTestFingerprint("original-parent"), promptSessionTestFingerprint("original-fork")
	policy := verifiedNewAPIPolicyContext{APIKeyID: 101, Meta: newAPIPolicyMeta{ThreadSource: "user", RequestKind: "turn", RootSessionFingerprint: child, ForkedFromSessionFingerprint: parent}}
	lookup := func(policy verifiedNewAPIPolicyContext) (int64, *database.WindowControlDiagnostic, error) {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/session-windows", nil)
		diagnostic := &database.WindowControlDiagnostic{}
		ctx.Set(windowControlDiagnosticContextKey, diagnostic)
		owner, _, err := handler.windowQuoteOwner(ctx, policy)
		return owner, diagnostic, err
	}
	owner, diagnostic, err := lookup(policy)
	require.Error(t, err)
	require.Zero(t, owner)
	require.Len(t, diagnostic.OwnerLookups, 2)
	parentLookup := diagnostic.OwnerLookups[1]
	require.Equal(t, "fork_parent", parentLookup.Target)
	require.Equal(t, "missing", parentLookup.Persistent)
	require.Equal(t, "missing", parentLookup.Live)
	require.Empty(t, parentLookup.ErrorKind)
	require.Equal(t, hashRiskIdentity(parent), parentLookup.RootHash)
	key := sessionAffinityKey("newapi-root-session:"+parent, 101)
	require.Equal(t, hashRiskIdentity(key), parentLookup.ScopeHash)
	encoded, err := json.Marshal(diagnostic)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), parent)
	require.NotContains(t, string(encoded), key)
	_, err = handler.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: 17, LastSeen: time.Now()})
	require.NoError(t, err)
	owner, diagnostic, err = lookup(policy)
	require.NoError(t, err)
	require.EqualValues(t, 17, owner)
	require.Equal(t, "found", diagnostic.OwnerLookups[1].Persistent)
	require.Equal(t, "not_checked", diagnostic.OwnerLookups[1].Live)
	policy.APIKeyID = 102
	_, diagnostic, err = lookup(policy)
	require.Error(t, err)
	require.Equal(t, parentLookup.RootHash, diagnostic.OwnerLookups[1].RootHash)
	require.NotEqual(t, parentLookup.ScopeHash, diagnostic.OwnerLookups[1].ScopeHash)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	failure := &database.WindowOwnerLookupDiagnostic{Persistent: "not_checked", Live: "not_checked"}
	_, _, err = handler.resolveForkSourceOwnerWithDiagnostic(canceled, requestSessionIdentity{forkSourceAffinityID: "newapi-root-session:" + parent}, "child", 101, failure)
	require.Error(t, err)
	require.Equal(t, "failed", failure.Persistent)
	require.Equal(t, "not_checked", failure.Live)
	require.Equal(t, "canceled", failure.ErrorKind)
}

func TestCodexInvalidTurnIdentityDiagnosticsRedactValues(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	handler := newWindowAuthorizationHandler(t)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	ctx := WithCodexIdentityStore(t.Context(), handler.db)
	for _, sample := range []struct {
		value, reason string
		version       int
	}{
		{"ffffffff-ffff-5fff-8fff-ffffffffffff", "unsupported_uuid_version", 5},
		{"private-access-token-do-not-log", "invalid_uuid", 0},
		{"ffffffff-ffff-7fff-ffff-ffffffffffff", "unsupported_uuid_variant", 7},
	} {
		headers, body := accountIdentityFixture(t, false, true)
		headers, body = setTurnIdentityTestFields(t, headers, body, sample.value, turnIdentitySample, true)
		fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
		err := fingerprint.ClaimSessionIdentity(ctx, account, "test-user-key")
		require.Error(t, err)
		require.NotContains(t, err.Error(), sample.value)
		failure := fingerprint.accountIdentityDiagnostic.InvalidTurnIdentity
		require.NotNil(t, failure)
		require.Equal(t, sample.reason, failure.Reason)
		require.Equal(t, sample.version, failure.UUIDVersion)
		require.Equal(t, hashRiskIdentity(sample.value), failure.ValueHash)
		require.Contains(t, failure.Sources, "client_metadata.turn_id")
		require.Contains(t, failure.Sources, "client_metadata.x-codex-turn-metadata.turn_id")
		require.Contains(t, failure.Sources, "headers.X-Codex-Turn-Metadata.turn_id")
		payload, err := json.Marshal(fingerprint.accountIdentityDiagnostic)
		require.NoError(t, err)
		require.NotContains(t, string(payload), sample.value)
	}
}
