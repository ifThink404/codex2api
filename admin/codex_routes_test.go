package admin

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexRouteSettingsRoundTrip(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	require.NoError(t, store.LoadAccountByID(t.Context(), id))
	h := &Handler{db: db, store: store}
	w := patchAccountScheduler(t, h, id, `{"codex_native_enabled":true,"codex_bps_enabled":true,"codex_native_models":["gpt-5.6-*"],"codex_bps_models":["gpt-6-astra"]}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	a := store.FindByID(id)
	require.True(t, a.CodexRouteAllows("native", "gpt-5.6-sol", false))
	require.False(t, a.CodexRouteAllows("native", "gpt-6-astra", false))
	require.True(t, a.CodexRouteAllows("bps", "gpt-6-astra", false))
	row, err := db.GetAccountByID(t.Context(), id)
	require.NoError(t, err)
	projection, err := db.ListAccountListProjection(t.Context(), "")
	require.NoError(t, err)
	for _, r := range projection {
		if r.ID == id {
			require.Equal(t, []string{"gpt-5.6-*"}, r.GetCredentialStringSlice(auth.CodexNativeModelsCredentialKey))
			require.True(t, r.GetCredentialBool(auth.CodexNativeEnabledCredentialKey))
		}
	}
	response := h.buildAccountResponse(row, a, nil, nil, nil, true)
	require.NotNil(t, response.CodexNativeEnabled)
	require.True(t, *response.CodexNativeEnabled)
	require.Equal(t, []string{"gpt-6-astra"}, response.CodexBPSModels)
	exported, ok := accountRowToCPAExportEntry(row, exportProxyResolver{})
	require.True(t, ok)
	encoded, err := json.Marshal(exported)
	require.NoError(t, err)
	var imported jsonAccountEntry
	require.NoError(t, json.Unmarshal(encoded, &imported))
	require.Equal(t, exported.CodexNativeEnabled, imported.CodexNativeEnabled)
	require.Equal(t, exported.CodexBPSModels, imported.CodexBPSModels)
	peer := auth.NewStore(db, nil, nil)
	defer peer.Stop()
	require.NoError(t, peer.LoadAccountByID(t.Context(), id))
	require.True(t, peer.FindByID(id).CodexRouteAllows("bps", "gpt-6-astra", false))
	require.True(t, accountFromCredentialSeed(id, "", tokenCredentialSeedFromAccountRow(row)).CodexRouteAllows("native", "gpt-5.6-sol", false))
	require.Equal(t, 400, patchAccountScheduler(t, h, id, `{"codex_native_models":["gpt*bad"]}`).Code)

	w = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/admin/accounts/batch-update", strings.NewReader(fmt.Sprintf(`{"ids":[%d],"codex_native_enabled":false,"codex_bps_models":[]}`, id)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.BatchUpdateAccounts(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.False(t, a.CodexRouteAllows("native", "gpt-5.6-sol", false))
	require.True(t, a.CodexRouteAllows("bps", "gpt-5.6-sol", false))
	row, err = db.GetAccountByID(t.Context(), id)
	require.NoError(t, err)
	require.False(t, row.GetCredentialBool(auth.CodexNativeEnabledCredentialKey))
	require.Empty(t, row.GetCredentialStringSlice(auth.CodexBPSModelsCredentialKey))
}

func TestCodexLegacyBatchSwitchesStayIndependent(t *testing.T) {
	db := newTestAdminDB(t)
	first, second := insertTestAccount(t, db), insertTestAccount(t, db)
	require.NoError(t, db.UpdateCredentials(t.Context(), second, map[string]interface{}{"codex_bps_enabled": true}))
	_, err := db.BatchUpdateAccountMetadata(t.Context(), []int64{first, second}, database.BatchAccountMetadataUpdate{CredentialUpdates: map[string]interface{}{"codex_bps_enabled": false}})
	require.NoError(t, err)
	for _, id := range []int64{first, second} {
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, auth.CodexNativeEnabledFromRow(row))
		require.Equal(t, id == first, row.GetCredentialBool(auth.CodexNativeEnabledCredentialKey))
	}
}

func TestSupportedModelsEditorSavesRoutesTogether(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	require.NoError(t, store.LoadAccountByID(t.Context(), id))
	h := &Handler{db: db, store: store}
	require.Equal(t, 200, patchAccountScheduler(t, h, id, `{"codex_native_enabled":true,"codex_bps_enabled":true}`).Code)
	patch := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
		c.Request = httptest.NewRequest("PATCH", "/accounts/models", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateAccountModels(c)
		return w
	}
	w := patch(`{"models":["gpt-5.6-sol","gpt-6-astra"],"codex_native_models":["gpt-5.6-*"],"codex_bps_models":["gpt-6-astra"]}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	a := store.FindByID(id)
	require.True(t, a.CodexRouteAllows("native", "gpt-5.6-sol", false))
	require.False(t, a.CodexRouteAllows("native", "gpt-6-astra", false))
	require.True(t, a.CodexRouteAllows("bps", "gpt-6-astra", false))
	require.Equal(t, 400, patch(`{"models":[],"codex_bps_models":["gpt*invalid"]}`).Code)
	row, err := db.GetAccountByID(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-6-astra"}, row.GetCredentialStringSlice("models"), "invalid route must not partially clear the common whitelist")
	require.Equal(t, []string{"gpt-6-astra"}, row.GetCredentialStringSlice(auth.CodexBPSModelsCredentialKey))
	require.Equal(t, 200, patch(`{"models":[],"codex_native_models":[]}`).Code)
	require.True(t, a.CodexRouteAllows("native", "gpt-6-astra", false), "empty route removes its extra restriction")
	require.False(t, a.CodexRouteAllows("bps", "gpt-5.6-sol", false), "omitted route is preserved")
	require.NoError(t, store.LoadAccountByID(t.Context(), id))
	require.True(t, store.FindByID(id).CodexRouteAllows("native", "gpt-6-astra", false))
	require.False(t, store.FindByID(id).CodexRouteAllows("bps", "gpt-5.6-sol", false))
}
