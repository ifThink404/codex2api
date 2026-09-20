package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexUAPreviewUsesServiceLogsAndRefreshesAfterCacheExpiry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ua.db"))
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = db.Close()
		}
	})
	handler := &Handler{db: db}
	config := `{"client_kind":"codex-desktop","client_version":"0.155.0-alpha.9","app_version":"26.915.31029"}`
	preview := func() proxy.CodexUserAgentPreview {
		t.Helper()
		payload, err := json.Marshal(codexUserAgentPreviewRequest{Config: config, ClientCompatMode: "force"})
		require.NoError(t, err)
		writer := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(writer)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/settings/codex-user-agent/preview", strings.NewReader(string(payload)))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.PreviewCodexUserAgent(ctx)
		require.Equal(t, http.StatusOK, writer.Code, writer.Body.String())
		var result proxy.CodexUserAgentPreview
		require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &result))
		return result
	}
	initial := preview()
	require.Equal(t, "empty", initial.Persona.Observation.Status)
	require.Empty(t, initial.Warnings)
	for _, entry := range []*database.UsageLogInput{
		{ClientUserAgent: initial.Persona.UserAgent, StatusCode: 200},
		{ClientUserAgent: "curl/9.0", UpstreamUserAgent: initial.Persona.UserAgent, StatusCode: 200},
	} {
		require.NoError(t, db.InsertUsageLog(context.Background(), entry))
	}
	db.FlushUsageLogs()
	require.Equal(t, "empty", preview().Persona.Observation.Status, "live traffic must not defeat the bounded preview cache")
	handler.codexUAObservations.expiresAt = time.Now().Add(-time.Second)
	updated := preview()
	require.Equal(t, "matched", updated.Persona.Observation.Status)
	require.EqualValues(t, 1, updated.Persona.Observation.MatchCount, "upstream UA must not seed inbound observations")
	require.Empty(t, updated.Warnings)
	require.Equal(t, initial.Persona.UserAgent, updated.Persona.UserAgent)
	require.Equal(t, initial.Normalized, updated.Normalized)
	require.NoError(t, db.Close())
	closed = true
	handler.codexUAObservations.expiresAt = time.Now().Add(-time.Second)
	failed := preview()
	require.Equal(t, "unavailable", failed.Persona.Observation.Status)
	require.Empty(t, failed.Warnings)
	require.Equal(t, initial.Persona.UserAgent, failed.Persona.UserAgent)
}
