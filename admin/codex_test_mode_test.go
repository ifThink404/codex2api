package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBPSBatchSettingsAndUnsupportedModeDoNotMutateAccounts(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	require.NoError(t, store.LoadAccountByID(t.Context(), id))
	h := &Handler{db: db, store: store}
	for _, enabled := range []bool{true, false} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("POST", "/api/admin/accounts/batch-update", strings.NewReader(fmt.Sprintf(`{"ids":[%d],"codex_bps_enabled":%t}`, id, enabled)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.BatchUpdateAccounts(c)
		require.Equal(t, 200, recorder.Code, recorder.Body.String())
		require.Equal(t, enabled, store.FindByID(id).CodexBPSEnabled())
	}
	ctx, err := proxy.WithCodexTestMode(t.Context(), "bps")
	require.NoError(t, err)
	relay := &auth.Account{DBID: 99, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "test", BaseURL: "https://relay.invalid"}
	status, message := h.runSingleBatchTest(ctx, relay)
	require.Equal(t, "failed", status)
	require.Contains(t, message, "不支持")
	require.False(t, relay.CodexBPS)
}

func TestTestResponsesHideBPSAddressesAndPublicErrors(t *testing.T) {
	for _, asJSON := range []bool{true, false} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Writer = testResponseWriter{c.Writer}
		if asJSON {
			c.JSON(http.StatusBadRequest, gin.H{"error": `Post https://bps.openai.com/basispoints/api/responses: connection lost`})
		} else {
			sendTestEvent(c, testEvent{Type: "error", Error: "tls failed: BPS.OPENAI.COM", CodexDiagnostics: &codexTestDiagnostics{ResponseBody: `{"nested":{"url":"https://bps.openai.com"}}`}})
		}
		require.NotContains(t, strings.ToLower(w.Body.String()), "bps.openai.com")
	}
	message := api.NewAPIErrorWithDetails(api.ErrCodeUpstreamError, "bps.openai.com", api.ErrorTypeUpstream, map[string]any{"nested": []string{`b\u0070s.openai.com`, "https://bps.openai.com"}})
	param := "bps.openai.com"
	message.Param = &param
	data, err := json.Marshal(message)
	require.NoError(t, err)
	require.NotContains(t, string(data), "bps.openai.com")
	require.True(t, json.Valid(data))
	// Local diagnostics retain the actual cause.
	require.Equal(t, "bps.openai.com", message.Message)
}
