package admin

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBatchRouteModelsPreserveUnselectedListsAndSwitches(t *testing.T) {
	db := newTestAdminDB(t)
	ids := []int64{insertTestAccount(t, db), insertTestAccount(t, db)}
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	for _, id := range ids {
		require.NoError(t, db.UpdateCredentials(t.Context(), id, map[string]interface{}{
			"models":               []string{"gpt-5.6-sol", "gpt-6-astra"},
			"codex_native_models":  []string{"gpt-old-native"},
			"codex_bps_models":     []string{"gpt-old-bps"},
			"codex_native_enabled": true, "codex_bps_enabled": false,
		}))
		require.NoError(t, store.LoadAccountByID(t.Context(), id))
	}
	h := &Handler{db: db, store: store}
	save := func(fields map[string]interface{}) *httptest.ResponseRecorder {
		fields["ids"] = ids
		body, err := json.Marshal(fields)
		require.NoError(t, err)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/accounts/batch-models", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.BatchUpdateAccountModels(c)
		return w
	}
	w := save(map[string]interface{}{
		"codex_native_models": []string{" gpt-5.6-* ", "gpt-5.6-*"},
		"codex_bps_models":    []string{"gpt-6-*"},
	})
	require.Equal(t, 200, w.Code, w.Body.String())
	for _, id := range ids {
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, []string{"gpt-5.6-sol", "gpt-6-astra"}, row.GetCredentialStringSlice("models"))
		require.Equal(t, []string{"gpt-5.6-*"}, row.GetCredentialStringSlice("codex_native_models"))
		require.Equal(t, []string{"gpt-6-*"}, row.GetCredentialStringSlice("codex_bps_models"))
		require.True(t, row.GetCredentialBool("codex_native_enabled"))
		require.False(t, row.GetCredentialBool("codex_bps_enabled"))
		require.Equal(t, []string{"gpt-5.6-*"}, store.FindByID(id).CodexNativeModels)
		require.Equal(t, []string{"gpt-6-*"}, store.FindByID(id).CodexBPSModels)
	}

	w = save(map[string]interface{}{"codex_bps_models": []string{}})
	require.Equal(t, 200, w.Code, w.Body.String())
	for _, id := range ids {
		require.NoError(t, store.LoadAccountByID(t.Context(), id))
		a := store.FindByID(id)
		require.Empty(t, a.CodexBPSModels)
		require.Equal(t, []string{"gpt-5.6-*"}, a.CodexNativeModels)
		require.NotEmpty(t, a.Models)
		require.False(t, a.CodexBPS)
	}

	w = save(map[string]interface{}{"models": []string{}, "codex_native_models": []string{"gpt*invalid"}})
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Equal(t, 400, save(map[string]interface{}{}).Code)
	for _, id := range ids {
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		require.NotEmpty(t, row.GetCredentialStringSlice("models"), "invalid route must not clear the whitelist")
		require.Equal(t, []string{"gpt-5.6-*"}, row.GetCredentialStringSlice("codex_native_models"))
	}
}

func TestBatchRouteModelsSkipIncompatibleAccounts(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, nil)
	ids := []int64{insertTestAccount(t, db), insertTestAccount(t, db), insertTestAccount(t, db)}
	accounts := []*auth.Account{
		{DBID: ids[0], AccessToken: "test-token"},
		{DBID: ids[1], UpstreamType: auth.UpstreamClaude},
		{DBID: ids[2], CodexAuthMode: auth.CodexAuthModeAgentIdentity, AgentRuntimeID: "test-runtime", AgentPrivateKey: "test-key"},
	}
	for _, a := range accounts {
		store.AddAccount(a)
	}
	h := &Handler{db: db, store: store}
	for _, field := range []string{"codex_bps_models", "codex_native_models"} {
		body, err := json.Marshal(map[string]interface{}{"ids": ids, field: []string{"gpt-6-*"}})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/accounts/batch-models", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.BatchUpdateAccountModels(c)
		require.Equal(t, 200, w.Code, w.Body.String())
		var result struct{ Success, Failed int }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		if field == "codex_bps_models" {
			require.Equal(t, 1, result.Success)
			require.Equal(t, 2, result.Failed)
			require.Empty(t, accounts[2].CodexBPSModels)
		} else {
			require.Equal(t, 2, result.Success)
			require.Equal(t, 1, result.Failed)
			require.Equal(t, []string{"gpt-6-*"}, accounts[2].CodexNativeModels)
		}
		row, err := db.GetAccountByID(t.Context(), ids[1])
		require.NoError(t, err)
		require.Empty(t, row.GetCredentialStringSlice(field), "Claude configuration must stay untouched")
	}
}
