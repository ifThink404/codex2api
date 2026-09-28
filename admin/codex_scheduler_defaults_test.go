package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNewCodexSchedulingDefaultsPersistAndMatchRuntime(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	restored := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(restored.Stop)
	for _, seed := range []tokenCredentialSeed{
		{refreshToken: "rt-defaults"},
		{accessToken: "at-defaults"},
		{sessionToken: "st-defaults"},
	} {
		id, err := h.insertNewCodexAccount(t.Context(), "defaults", "", &seed)
		require.NoError(t, err)
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		for _, key := range []string{auth.CodexBPSEnabledCredentialKey, auth.CodexBPSImageTrimCredentialKey, auth.CodexUsageLimitBypassEnabledKey} {
			require.True(t, row.GetCredentialBool(key), key)
		}
		require.Equal(t, []string{"gpt-5.6-sol"}, row.GetCredentialStringSlice(auth.CodexUsageLimitBypassModelsKey))
		account := h.newCodexAccountFromSeed(id, "", seed)
		require.True(t, row.SkipWarmTier)
		require.True(t, account.SkipWarmTier)
		require.False(t, row.GetCredentialBool(auth.CodexNativeEnabledCredentialKey))
		require.True(t, account.CodexBPS)
		require.NotNil(t, account.CodexNative)
		require.False(t, *account.CodexNative)
		require.True(t, account.CodexBPSImageTrim)
		require.True(t, account.CodexUsageLimitBypassEnabled)
		require.Equal(t, []string{"gpt-5.6-sol"}, account.CodexUsageLimitBypassModels)
		require.NoError(t, restored.LoadAccountByID(t.Context(), id))
		reloaded := restored.FindByID(id)
		require.NotNil(t, reloaded)
		require.True(t, reloaded.SkipWarmTier)
		require.NotNil(t, reloaded.CodexNative)
		require.False(t, *reloaded.CodexNative)
		require.True(t, reloaded.CodexBPS)
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
			require.Equal(t, false, credentials[auth.CodexNativeEnabledCredentialKey])
			for _, key := range []string{auth.CodexBPSEnabledCredentialKey, auth.CodexBPSImageTrimCredentialKey, auth.CodexUsageLimitBypassEnabledKey} {
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

func TestImportedCodexWarmTierDefaultAndExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		warm, native  bool
	}{
		{"missing", `[{"refresh_token":"rt-new"}]`, true, false},
		{"bps_disabled", `[{"refresh_token":"rt-new","codex_bps_enabled":false}]`, true, false},
		{"explicit", `[{"refresh_token":"rt-new","skip_warm_tier":false,"codex_native_enabled":true}]`, false, true},
		{"nested", `{"accounts":[{"credentials":{"refresh_token":"rt-new","skip_warm_tier":false,"codex_native_enabled":true}}]}`, false, true},
		{"root_precedence", `{"accounts":[{"skip_warm_tier":false,"credentials":{"refresh_token":"rt-new","skip_warm_tier":true}}]}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestAdminDB(t)
			h := &Handler{db: db}
			tokens, err := parseImportJSONTokens([]byte(tc.payload))
			require.NoError(t, err)
			require.Len(t, tokens, 1)
			seed := importTokenSeed(tokens[0], nil)
			id, err := h.insertNewCodexAccount(t.Context(), "import", "", &seed)
			require.NoError(t, err)
			row, err := db.GetAccountByID(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, tc.warm, row.SkipWarmTier)
			require.Equal(t, tc.native, row.GetCredentialBool(auth.CodexNativeEnabledCredentialKey))
			require.Equal(t, tc.warm, h.newCodexAccountFromSeed(id, "", seed).SkipWarmTier)
			entry, ok := accountRowToCPAExportEntry(row, exportProxyResolver{})
			require.True(t, ok)
			raw, err := json.Marshal(entry)
			require.NoError(t, err)
			exported, err := parseImportJSONTokens(raw)
			require.NoError(t, err)
			require.Len(t, exported, 1)
			roundtrip := importTokenSeed(exported[0], nil)
			h.newCodexAccountCredentials(&roundtrip)
			require.Equal(t, tc.warm, *roundtrip.skipWarmTier)
			require.Equal(t, tc.native, *roundtrip.codexNativeEnabled)
			// Refreshes preserve the saved scheduler settings.
			require.NoError(t, db.UpdateOAuthAccountCredentials(t.Context(), id, tokenCredentialMap(tokenCredentialSeed{refreshToken: "rt-refreshed"}), ""))
			row, err = db.GetAccountByID(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, tc.warm, row.SkipWarmTier)
			require.Equal(t, tc.native, row.GetCredentialBool(auth.CodexNativeEnabledCredentialKey))
		})
	}
}

func TestAddCodexAccountsAppliesDefaultsImmediately(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	h := &Handler{db: db, store: store}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts", strings.NewReader(`{"refresh_token":"rt-default-a\nrt-default-b","skip_refresh":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.AddAccount(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	rows, err := db.ListActive(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.True(t, row.SkipWarmTier)
		require.False(t, row.GetCredentialBool(auth.CodexNativeEnabledCredentialKey))
		account := store.FindByID(row.ID)
		require.NotNil(t, account)
		require.True(t, account.SkipWarmTier)
		require.NotNil(t, account.CodexNative)
		require.False(t, *account.CodexNative)
		require.True(t, account.CodexBPS)
	}
}
