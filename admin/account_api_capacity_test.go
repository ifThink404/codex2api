package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAPIAccountCapacitySchedulerPersistsAndDisplays(t *testing.T) {
	db := newTestAdminDB(t)
	id, err := db.InsertOpenAIResponsesAccount(t.Context(), "API windows", map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses, "base_url": "http://relay.invalid", "api_key": "test-key",
	}, "")
	require.NoError(t, err)
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "http://relay.invalid", APIKey: "test-key", Status: auth.StatusReady}
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}
	update := func(body string, batch bool) {
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
		request.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/scheduler", strings.NewReader(body))
		request.Request.Header.Set("Content-Type", "application/json")
		if batch {
			handler.BatchUpdateAccounts(request)
		} else {
			handler.UpdateAccountScheduler(request)
		}
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	}
	update(`{"session_capacity_enabled":true,"session_capacity_max":3,"session_capacity_idle_ttl_seconds":7200}`, false)
	enabled, limit, ttl := account.SessionCapacityConfig()
	require.True(t, enabled)
	require.EqualValues(t, 3, limit)
	require.Equal(t, 2*time.Hour, ttl)
	update(fmt.Sprintf(`{"ids":[%d],"session_capacity_max":4}`, id), true)
	enabled, limit, ttl = account.SessionCapacityConfig()
	require.True(t, enabled)
	require.EqualValues(t, 4, limit)
	require.Equal(t, 2*time.Hour, ttl, "partial updates must preserve the existing TTL")
	require.True(t, store.AdmitAccountSession(account, "api-root", time.Now()))
	row, err := db.GetAccountByID(t.Context(), id)
	require.NoError(t, err)
	require.True(t, row.GetCredentialBool(auth.SessionCapacityEnabledCredentialKey))
	value, ok := row.GetCredentialInt64(auth.SessionCapacityMaxCredentialKey)
	require.True(t, ok)
	require.EqualValues(t, 4, value)
	for _, detail := range []bool{false, true} {
		response := handler.buildAccountResponse(row, account, nil, nil, nil, detail)
		require.True(t, response.SessionCapacityEnabled)
		require.EqualValues(t, 4, response.SessionCapacityMax)
		require.EqualValues(t, 7200, response.SessionCapacityIdleTTLSeconds)
		require.EqualValues(t, 1, response.SessionCapacityCurrent)
		require.Empty(t, response.CodexFingerprintMode, "API relays must not acquire native-only fingerprint settings")
	}
	update(`{"session_capacity_enabled":false}`, false)
	enabled, _, _ = account.SessionCapacityConfig()
	require.False(t, enabled)
	total, _ := store.AccountSessionSlotCounts(id, time.Now())
	require.Zero(t, total)
}
