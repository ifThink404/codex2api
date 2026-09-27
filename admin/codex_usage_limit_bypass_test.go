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

func TestUsageLimitBypassSettingsAndBatch(t *testing.T) {
	db := newTestAdminDB(t)
	first, second := insertTestAccount(t, db), insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	h := &Handler{db: db, store: store}
	for _, id := range []int64{first, second} {
		require.NoError(t, store.LoadAccountByID(t.Context(), id))
	}
	policy := auth.DispatchPolicyStandard.WithModel("gpt-6-astra")
	require.False(t, store.FindByID(first).UsageLimitBypassMatches(policy))
	patch := `{"codex_usage_limit_bypass_enabled":true,"codex_usage_limit_bypass_models":["gpt-6-astra","gpt-5.6-sol"]}`
	w := patchAccountScheduler(t, h, first, patch)
	require.Equal(t, 200, w.Code, w.Body.String())
	check := func(id int64, enabled bool, models []string) {
		t.Helper()
		row, err := db.GetAccountByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, enabled, row.GetCredentialBool(auth.CodexUsageLimitBypassEnabledKey))
		require.Equal(t, models, row.GetCredentialStringSlice(auth.CodexUsageLimitBypassModelsKey))
		response := h.buildAccountResponse(row, store.FindByID(id), nil, nil, nil, true)
		require.Equal(t, enabled, response.CodexUsageLimitBypassEnabled)
		require.Equal(t, models, response.CodexUsageLimitBypassModels)
		rows, err := db.ListAccountListProjection(t.Context(), "")
		require.NoError(t, err)
		for _, r := range rows {
			if r.ID == id {
				require.Equal(t, enabled, r.GetCredentialBool(auth.CodexUsageLimitBypassEnabledKey))
				require.Equal(t, models, r.GetCredentialStringSlice(auth.CodexUsageLimitBypassModelsKey))
			}
		}
		peer := auth.NewStore(db, nil, nil)
		require.NoError(t, peer.LoadAccountByID(t.Context(), id))
		require.Equal(t, enabled && len(models) > 0, peer.FindByID(id).UsageLimitBypassMatches(policy))
		peer.Stop()
		exported, ok := accountRowToCPAExportEntry(row, exportProxyResolver{})
		require.True(t, ok)
		data, err := json.Marshal(exported)
		require.NoError(t, err)
		var imported jsonAccountEntry
		require.NoError(t, json.Unmarshal(data, &imported))
		require.NotNil(t, imported.CodexUsageLimitBypassEnabled)
		require.Equal(t, enabled, *imported.CodexUsageLimitBypassEnabled)
		require.Equal(t, models, imported.CodexUsageLimitBypassModels)
		rebuilt := accountFromCredentialSeed(id, "", tokenCredentialSeedFromAccountRow(row))
		require.Equal(t, enabled && len(models) > 0, rebuilt.UsageLimitBypassMatches(policy))
	}
	models := []string{"gpt-5.6-sol", "gpt-6-astra"}
	check(first, true, models)
	// Unrelated edits and disabling preserve the model list.
	require.Equal(t, 200, patchAccountScheduler(t, h, first, `{"codex_native_enabled":true}`).Code)
	check(first, true, models)
	require.Equal(t, 200, patchAccountScheduler(t, h, first, `{"codex_usage_limit_bypass_enabled":false}`).Code)
	check(first, false, models)
	batch := func(payload string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/api/admin/accounts/batch-update", strings.NewReader(payload))
		h.BatchUpdateAccounts(c)
		return w
	}
	w = batch(fmt.Sprintf(`{"ids":[%d,%d],%s`, first, second, strings.TrimPrefix(patch, "{")))
	require.Equal(t, 200, w.Code, w.Body.String())
	check(first, true, models)
	check(second, true, models)
	for _, wildcard := range []string{"*", "gpt-6-*"} {
		invalid := fmt.Sprintf(`"codex_usage_limit_bypass_models":[%q]`, wildcard)
		require.Equal(t, 400, patchAccountScheduler(t, h, first, "{"+invalid+"}").Code)
		require.Equal(t, 400, batch(fmt.Sprintf(`{"ids":[%d,%d],%s}`, first, second, invalid)).Code)
	}
	for _, payload := range []string{`{"codex_usage_limit_bypass_enabled":"true"}`, `{"codex_usage_limit_bypass_models":"gpt-6-astra"}`, `{"codex_usage_limit_bypass_models":["gpt-*astra"]}`, `{"codex_usage_limit_bypass_models":[true]}`} {
		require.Equal(t, 400, patchAccountScheduler(t, h, first, payload).Code)
	}
	relay, err := db.InsertOpenAIResponsesAccount(t.Context(), "relay", map[string]any{"upstream_type": auth.UpstreamOpenAIResponses, "api_key": "test", "base_url": "https://relay.invalid"}, "")
	require.NoError(t, err)
	require.Equal(t, 400, patchAccountScheduler(t, h, relay, patch).Code)
	require.Equal(t, 400, batch(fmt.Sprintf(`{"ids":[%d,%d],"codex_usage_limit_bypass_enabled":false}`, first, relay)).Code)
	check(first, true, models)
	require.Equal(t, 200, patchAccountScheduler(t, h, first, `{"codex_usage_limit_bypass_models":[]}`).Code)
	require.False(t, store.FindByID(first).UsageLimitBypassMatches(policy))
}
