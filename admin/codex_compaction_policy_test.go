package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNativeCompactionOnlySettingRoundTrip(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	require.NoError(t, store.LoadAccountByID(t.Context(), id))
	h := &Handler{db: db, store: store}
	require.False(t, store.FindByID(id).NativeCompactionOnlyEnabled())
	for _, enabled := range []bool{true, false} {
		response := patchAccountScheduler(t, h, id, fmt.Sprintf(`{"codex_native_compaction_only":%t}`, enabled))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, enabled, store.FindByID(id).NativeCompactionOnlyEnabled())
		require.False(t, store.FindByID(id).CodexBPSEnabled())
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, enabled, row.GetCredentialBool(auth.CodexNativeCompactionOnlyCredentialKey))
		require.Equal(t, enabled, h.buildAccountResponse(row, store.FindByID(id), nil, nil, nil, true).CodexNativeCompactionOnly)
		projection, err := db.ListAccountListProjection(t.Context(), "")
		require.NoError(t, err)
		found := false
		for _, r := range projection {
			if r.ID == id {
				found = true
				require.Equal(t, enabled, r.GetCredentialBool(auth.CodexNativeCompactionOnlyCredentialKey))
			}
		}
		require.True(t, found)
		peer := auth.NewStore(db, nil, nil)
		require.NoError(t, peer.LoadAccountByID(t.Context(), id))
		require.Equal(t, enabled, peer.FindByID(id).NativeCompactionOnlyEnabled())
		peer.Stop()
		exported, ok := accountRowToCPAExportEntry(row, exportProxyResolver{})
		require.True(t, ok)
		require.Equal(t, enabled, exported.CodexNativeCompactionOnly)
		encoded, err := json.Marshal(exported)
		require.NoError(t, err)
		var imported jsonAccountEntry
		require.NoError(t, json.Unmarshal(encoded, &imported))
		require.Equal(t, enabled, imported.CodexNativeCompactionOnly)
		seed := tokenCredentialSeedFromAccountRow(row)
		require.Equal(t, enabled, accountFromCredentialSeed(id, "", seed).NativeCompactionOnlyEnabled())
	}
	require.Equal(t, http.StatusBadRequest, patchAccountScheduler(t, h, id, `{"codex_native_compaction_only":"true"}`).Code)
	for _, enabled := range []bool{true, false} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/api/admin/accounts/batch-update", strings.NewReader(fmt.Sprintf(`{"ids":[%d],"codex_native_compaction_only":%t}`, id, enabled)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.BatchUpdateAccounts(c)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, enabled, store.FindByID(id).NativeCompactionOnlyEnabled())
	}
	// Unrelated edits leave the saved value intact.
	require.Equal(t, 200, patchAccountScheduler(t, h, id, `{"codex_native_compaction_only":true}`).Code)
	require.Equal(t, 200, patchAccountScheduler(t, h, id, `{"codex_bps_enabled":true}`).Code)
	require.True(t, store.FindByID(id).NativeCompactionOnlyEnabled())
}
