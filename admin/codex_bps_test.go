package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestBPSAccountSettingSaveReloadAndExport(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	require.NoError(t, store.LoadAccountByID(t.Context(), id))
	h := &Handler{db: db, store: store}
	require.False(t, store.FindByID(id).CodexBPSEnabled())
	for _, enabled := range []bool{true, false} {
		body, _ := json.Marshal(map[string]bool{"codex_bps_enabled": enabled})
		response := patchAccountScheduler(t, h, id, string(body))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, enabled, store.FindByID(id).CodexBPSEnabled())
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, enabled, row.GetCredentialBool(auth.CodexBPSEnabledCredentialKey))
		require.Equal(t, enabled, h.buildAccountResponse(row, store.FindByID(id), nil, nil, nil, true).CodexBPSEnabled)
		projected, err := db.ListAccountListProjection(t.Context(), "")
		require.NoError(t, err)
		require.NotEmpty(t, projected)
		for _, item := range projected {
			if item.ID == id {
				require.Equal(t, enabled, item.GetCredentialBool(auth.CodexBPSEnabledCredentialKey))
			}
		}
		peer := auth.NewStore(db, nil, nil)
		require.NoError(t, peer.LoadAccountByID(t.Context(), id))
		require.Equal(t, enabled, peer.FindByID(id).CodexBPSEnabled())
		peer.Stop()
		exported, ok := accountRowToCPAExportEntry(row, exportProxyResolver{})
		require.True(t, ok)
		require.Equal(t, enabled, exported.CodexBPSEnabled)
		exportedJSON, err := json.Marshal(exported)
		require.NoError(t, err)
		var imported jsonAccountEntry
		require.NoError(t, json.Unmarshal(exportedJSON, &imported))
		require.NotNil(t, imported.CodexBPSEnabled)
		require.Equal(t, enabled, *imported.CodexBPSEnabled)
		seed := tokenCredentialSeedFromAccountRow(row)
		require.Equal(t, enabled, accountFromCredentialSeed(id, "", seed).CodexBPSEnabled())
	}
	invalid := patchAccountScheduler(t, h, id, `{"codex_bps_enabled":"true"}`)
	require.Equal(t, http.StatusBadRequest, invalid.Code)
}

func TestBPSAccountSettingRejectsRelay(t *testing.T) {
	db := newTestAdminDB(t)
	id, err := db.InsertOpenAIResponsesAccount(t.Context(), "relay", map[string]any{"upstream_type": auth.UpstreamOpenAIResponses, "base_url": "https://relay.invalid", "api_key": "test"}, "")
	require.NoError(t, err)
	h := &Handler{db: db}
	response := patchAccountScheduler(t, h, id, `{"codex_bps_enabled":true}`)
	require.Equal(t, http.StatusBadRequest, response.Code)
	row, err := db.GetAccountByID(t.Context(), id)
	require.NoError(t, err)
	require.False(t, row.GetCredentialBool(auth.CodexBPSEnabledCredentialKey))
}
