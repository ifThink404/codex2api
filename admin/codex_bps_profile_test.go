package admin

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBPSProfileSettingsPersistenceAndBatch(t *testing.T) {
	db := newTestAdminDB(t)
	first, second := insertTestAccount(t, db), insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	h := &Handler{db: db, store: store}
	for _, id := range []int64{first, second} {
		require.NoError(t, store.LoadAccountByID(t.Context(), id))
	}
	require.Equal(t, auth.BPSWord, store.FindByID(first).EffectiveCodexBPSProfile())
	for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		w := patchAccountScheduler(t, h, first, fmt.Sprintf(`{"codex_bps_profile":%q}`, profile))
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, profile, store.FindByID(first).EffectiveCodexBPSProfile())
		require.False(t, store.FindByID(first).CodexBPSEnabled())
		row, err := db.GetAccountByID(t.Context(), first)
		require.NoError(t, err)
		require.Equal(t, string(profile), row.GetCredential(auth.CodexBPSProfileCredentialKey))
		require.Equal(t, profile, h.buildAccountResponse(row, store.FindByID(first), nil, nil, nil, true).CodexBPSProfile)
		rows, err := db.ListAccountListProjection(t.Context(), "")
		require.NoError(t, err)
		found := false
		for _, r := range rows {
			if r.ID == first {
				found = true
				require.Equal(t, string(profile), r.GetCredential(auth.CodexBPSProfileCredentialKey))
			}
		}
		require.True(t, found)
		peer := auth.NewStore(db, nil, nil)
		require.NoError(t, peer.LoadAccountByID(t.Context(), first))
		require.Equal(t, profile, peer.FindByID(first).EffectiveCodexBPSProfile())
		peer.Stop()
		exported, ok := accountRowToCPAExportEntry(row, exportProxyResolver{})
		require.True(t, ok)
		data, err := json.Marshal(exported)
		require.NoError(t, err)
		var imported jsonAccountEntry
		require.NoError(t, json.Unmarshal(data, &imported))
		require.Equal(t, profile, imported.CodexBPSProfile)
		require.Equal(t, profile, accountFromCredentialSeed(first, "", tokenCredentialSeedFromAccountRow(row)).EffectiveCodexBPSProfile())
	}
	batch := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/api/admin/accounts/batch-update", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.BatchUpdateAccounts(c)
		return w
	}
	w := batch(fmt.Sprintf(`{"ids":[%d,%d],"codex_bps_profile":"sheets"}`, first, second))
	require.Equal(t, 200, w.Code, w.Body.String())
	for _, id := range []int64{first, second} {
		require.Equal(t, auth.BPSSheets, store.FindByID(id).EffectiveCodexBPSProfile())
	}
	w = batch(fmt.Sprintf(`{"ids":[%d,%d],"codex_bps_enabled":true}`, first, second))
	require.Equal(t, 200, w.Code, w.Body.String())
	for _, id := range []int64{first, second} {
		require.Equal(t, auth.BPSSheets, store.FindByID(id).EffectiveCodexBPSProfile())
	}
	for _, invalid := range []string{`null`, `""`, `"Word"`, `true`, `["word","excel"]`, `"unknown"`} {
		require.Equal(t, 400, patchAccountScheduler(t, h, first, `{"codex_bps_profile":`+invalid+`}`).Code)
		require.Equal(t, 400, batch(fmt.Sprintf(`{"ids":[%d,%d],"codex_bps_profile":%s}`, first, second, invalid)).Code)
		require.Equal(t, auth.BPSSheets, store.FindByID(first).EffectiveCodexBPSProfile())
	}
	// A mixed batch must fail before modifying any eligible account.
	relay, err := db.InsertOpenAIResponsesAccount(t.Context(), "relay", map[string]any{"upstream_type": auth.UpstreamOpenAIResponses, "base_url": "https://relay.invalid", "api_key": "test"}, "")
	require.NoError(t, err)
	require.Equal(t, 400, patchAccountScheduler(t, h, relay, `{"codex_bps_profile":"excel"}`).Code)
	require.Equal(t, 400, batch(fmt.Sprintf(`{"ids":[%d,%d],"codex_bps_profile":"excel"}`, first, relay)).Code)
	require.Equal(t, auth.BPSSheets, store.FindByID(first).EffectiveCodexBPSProfile())
}
