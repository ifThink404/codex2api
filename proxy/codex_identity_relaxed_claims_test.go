package proxy

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRelaxedIdentityClaimsIsolateOwnerWithoutTakingOverLegacyClaim(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	originalSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { storeRuntimeSettings(originalSettings) })
	settings := originalSettings
	settings.CodexForkAccountFallbackEnabled = false
	storeRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claims.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	h := &Handler{db: db}
	a := &auth.Account{DBID: 7, AccountID: accountIdentitySampleAccount}
	headers, body := accountIdentityFixture(t, false, true)
	first := codexOwnerTestContext(t, h, "old-user", true)
	fp := NewCodexTransportFingerprint(a, headers, body, "cache")
	require.NoError(t, fp.ClaimSessionIdentity(first.Request.Context(), a, "shared-key"))
	oldRoot := gjson.GetBytes(fp.ApplyBody(body), "client_metadata.session_id").String()
	second := codexOwnerTestContext(t, h, "new-user", true)
	attachUpstreamTrace(second, nil)
	claim := func(headers http.Header, body []byte) (*CodexFingerprint, error) {
		fp := NewCodexTransportFingerprint(a, headers, body, "cache")
		return fp, fp.ClaimSessionIdentity(second.Request.Context(), a, "shared-key")
	}
	_, err = claim(headers, body)
	require.Equal(t, "codex_session_identity_conflict", string(codexIdentityRequestError(err).Code))
	strict := snapshotUpstreamTrace(second.Request.Context()).IdentityClaim
	require.Equal(t, "conflict", strict.Result)
	require.Equal(t, "signed_newapi", strict.OwnerSource)
	require.NotEmpty(t, strict.ExistingOwnerHash)
	require.NotEqual(t, strict.ExistingOwnerHash, strict.OwnerHash)
	settings.CodexForkAccountFallbackEnabled = true
	storeRuntimeSettings(settings)
	fp, err = claim(headers, body)
	require.NoError(t, err)
	newRoot := gjson.GetBytes(fp.ApplyBody(body), "client_metadata.session_id").String()
	require.NotEqual(t, oldRoot, newRoot)
	require.NotEqual(t, accountIdentitySampleRoot, newRoot)
	require.Equal(t, "isolated", snapshotUpstreamTrace(second.Request.Context()).IdentityClaim.Result)
	repeated, err := claim(headers, body)
	require.NoError(t, err)
	require.Equal(t, newRoot, gjson.GetBytes(repeated.ApplyBody(body), "client_metadata.session_id").String())
	// Parent references must retain the new owner's isolated root mapping.
	childHeaders, childBody := accountIdentityFixture(t, true, true)
	child, err := claim(childHeaders, childBody)
	require.NoError(t, err)
	require.Equal(t, newRoot, gjson.GetBytes(child.ApplyBody(childBody), "client_metadata.parent_thread_id").String())
	childAgain, err := claim(childHeaders, childBody)
	require.NoError(t, err)
	require.JSONEq(t, string(child.ApplyBody(childBody)), string(childAgain.ApplyBody(childBody)))
	// A subagent may be the first request after signing is enabled or a user
	// credential changes; it must not require a warmed-up main-thread mapping.
	third := codexOwnerTestContext(t, h, "fresh-subagent-user", true)
	freshChild := NewCodexTransportFingerprint(a, childHeaders, childBody, "cache")
	require.NoError(t, freshChild.ClaimSessionIdentity(third.Request.Context(), a, "shared-key"))
	require.NotEqual(t, oldRoot, gjson.GetBytes(freshChild.ApplyBody(childBody), "client_metadata.parent_thread_id").String())
	// The old user still owns their claim and their existing outbound IDs.
	old := NewCodexTransportFingerprint(a, headers, body, "cache")
	require.NoError(t, old.ClaimSessionIdentity(first.Request.Context(), a, "shared-key"))
	require.Equal(t, oldRoot, gjson.GetBytes(old.ApplyBody(body), "client_metadata.session_id").String())
	settings.CodexForkAccountFallbackEnabled = false
	storeRuntimeSettings(settings)
	_, err = claim(headers, body)
	require.Equal(t, "codex_session_identity_conflict", string(codexIdentityRequestError(err).Code))
	resetUpstreamRequestTrace(second)
	require.Nil(t, snapshotUpstreamTrace(second.Request.Context()).IdentityClaim)
}
