package admin

import (
	"encoding/json"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestNewCodexSchedulingDefaultsPersistAndMatchRuntime(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	for _, seed := range []tokenCredentialSeed{
		{refreshToken: "rt-defaults"},
		{accessToken: "at-defaults"},
		{sessionToken: "st-defaults"},
	} {
		credentials := h.newCodexAccountCredentials(&seed)
		id, err := db.InsertAccountWithCredentials(t.Context(), "defaults", credentials, "")
		require.NoError(t, err)
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		for _, key := range []string{auth.CodexBPSEnabledCredentialKey, auth.CodexNativeEnabledCredentialKey, auth.CodexBPSImageTrimCredentialKey, auth.CodexUsageLimitBypassEnabledKey} {
			require.True(t, row.GetCredentialBool(key), key)
		}
		require.Equal(t, []string{"gpt-5.6-sol"}, row.GetCredentialStringSlice(auth.CodexUsageLimitBypassModelsKey))
		account := h.newCodexAccountFromSeed(id, "", seed)
		require.True(t, account.CodexBPS)
		require.NotNil(t, account.CodexNative)
		require.True(t, *account.CodexNative)
		require.True(t, account.CodexBPSImageTrim)
		require.True(t, account.CodexUsageLimitBypassEnabled)
		require.Equal(t, []string{"gpt-5.6-sol"}, account.CodexUsageLimitBypassModels)
	}
}

func TestImportedCodexSchedulingDefaultsRespectExplicitValues(t *testing.T) {
	h := &Handler{}
	for _, wrap := range []func(string) string{
		func(fields string) string { return `[{"refresh_token":"rt-import"` + fields + `}]` },
		func(fields string) string {
			return `{"accounts":[{"credentials":{"refresh_token":"rt-import"` + fields + `}}]}`
		},
	} {
		for _, explicit := range []bool{false, true} {
			fields := ""
			if explicit {
				fields = `,"codex_bps_enabled":false,"codex_native_enabled":false,"codex_bps_image_trim_enabled":false,"codex_usage_limit_bypass_enabled":false,"codex_usage_limit_bypass_models":[]`
			}
			tokens, err := parseImportJSONTokens([]byte(wrap(fields)))
			require.NoError(t, err)
			require.Len(t, tokens, 1)
			seed := importTokenSeed(tokens[0], nil)
			credentials := h.newCodexAccountCredentials(&seed)
			for _, key := range []string{auth.CodexBPSEnabledCredentialKey, auth.CodexNativeEnabledCredentialKey, auth.CodexBPSImageTrimCredentialKey, auth.CodexUsageLimitBypassEnabledKey} {
				require.Equal(t, !explicit, credentials[key], key)
			}
			models := credentials[auth.CodexUsageLimitBypassModelsKey]
			if explicit {
				require.Equal(t, []string{}, models)
			} else {
				require.Equal(t, []string{"gpt-5.6-sol"}, models)
			}
		}
	}
}

func TestCodexSchedulingDefaultsDoNotApplyToCredentialRefresh(t *testing.T) {
	credentials := tokenCredentialMap(tokenCredentialSeed{refreshToken: "rt-refresh"})
	for _, key := range []string{auth.CodexBPSEnabledCredentialKey, auth.CodexNativeEnabledCredentialKey, auth.CodexBPSImageTrimCredentialKey, auth.CodexUsageLimitBypassEnabledKey, auth.CodexUsageLimitBypassModelsKey} {
		require.NotContains(t, credentials, key)
	}
}

func TestImportedBPSOnlyAccountPreservesImplicitNativeRoute(t *testing.T) {
	tokens, err := parseImportJSONTokens([]byte(`[{"refresh_token":"rt-bps-only","codex_bps_enabled":true}]`))
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	seed := importTokenSeed(tokens[0], nil)
	credentials := (&Handler{}).newCodexAccountCredentials(&seed)
	require.Equal(t, true, credentials[auth.CodexBPSEnabledCredentialKey])
	require.Equal(t, false, credentials[auth.CodexNativeEnabledCredentialKey])
}

func TestCodexExportPreservesDisabledSchedulingSettings(t *testing.T) {
	data, err := json.Marshal([]cpaExportEntry{{RefreshToken: "rt-export", CodexUsageLimitBypassModels: []string{}}})
	require.NoError(t, err)
	tokens, err := parseImportJSONTokens(data)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	seed := importTokenSeed(tokens[0], nil)
	credentials := (&Handler{}).newCodexAccountCredentials(&seed)
	for _, key := range []string{auth.CodexBPSEnabledCredentialKey, auth.CodexBPSImageTrimCredentialKey, auth.CodexUsageLimitBypassEnabledKey} {
		require.Equal(t, false, credentials[key], key)
	}
}
