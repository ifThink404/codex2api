package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestProjectProtocolDiagnosticCapture(t *testing.T) {
	const raw = `{"project_id":"` + projectTestID + `","projectId":"` + projectTestID + `","workspace_id":"` + unrelatedProjectUUID + `","ProjectID":"` + projectTestID + `","token":"private-token","input":"private-prompt"}`
	headers := http.Header{}
	headers.Set("X-Codex-Project-Id", projectTestID)
	headers.Set("X-Codex-Workspace-Id", unrelatedProjectUUID)
	headers.Set(codexTurnMetadataHeader, raw)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header = headers
	for _, captured := range []map[string]string{captureUsageDiagnosticHeaders(c), CaptureOutboundIdentityHeaders(headers).Headers} {
		require.Equal(t, projectTestID, captured["X-Codex-Project-Id"])
		require.Equal(t, unrelatedProjectUUID, captured["X-Codex-Workspace-Id"])
	}
	for _, embedded := range []any{json.RawMessage(raw), raw} {
		body, err := json.Marshal(map[string]any{"client_metadata": map[string]any{"project_id": projectTestID, "x-codex-turn-metadata": embedded}})
		require.NoError(t, err)
		c.Set(usageRequestDiagnosticsContextKey, nil)
		captureUsageRequestIngress(c, body)
		state := usageRequestDiagnosticState(c)
		require.Equal(t, projectTestID, state.Incoming["client_metadata"]["project_id"])
		encoded, err := json.Marshal(captureOutboundIdentityBody(body))
		require.NoError(t, err)
		require.Equal(t, projectTestID, gjson.GetBytes(encoded, "client_metadata.project_id").String())
		metadata := diagnosticMetadataObject(gjson.GetBytes(encoded, "client_metadata.x-codex-turn-metadata"))
		require.Equal(t, projectTestID, metadata.Get("ProjectID").String())
		require.Equal(t, unrelatedProjectUUID, metadata.Get("workspace_id").String())
		require.NotContains(t, string(encoded), "private-")
	}
}

func TestProjectDiagnosticBoundsDoNotLimitMapping(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	ids := make([]string, projectDiagnosticMaxChanges+3)
	items := make([]any, len(ids))
	for i := range ids {
		ids[i] = uuid.NewString()
		items[i] = map[string]any{"project_id": ids[i], "text": fmt.Sprintf("project_id: %s project_id: %s", ids[i], ids[i])}
	}
	body, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)
	c, _, _ := responsePrivacyRequest(t, h, 101, "diagnostic-bounds", "")
	ctx, mapped, err := PrepareCodexProjectOutbound(c.Request.Context(), account, body, nil)
	require.NoError(t, err)
	s := projectIdentityFrom(ctx)
	require.Len(t, s.changes, len(ids))
	for _, id := range ids {
		require.NotContains(t, string(mapped), id)
		require.Contains(t, string(mapped), s.forward[id])
		// Duplicate discovery paths and long nested keys cannot bloat the log.
		s.changes[id].Sources = []string{"body.input[0].project_id", "body.input[0].project_id", strings.Repeat("界", 400), "body.input[1].project_id"}
	}
	before, err := json.Marshal(s.changes)
	require.NoError(t, err)
	d := s.diagnostic()
	require.Len(t, d.Changes, projectDiagnosticMaxChanges)
	require.Equal(t, 3, d.OmittedChanges)
	for _, change := range d.Changes {
		require.Len(t, change.Sources, 2)
		require.True(t, change.SourcesTruncated)
		require.LessOrEqual(t, len([]rune(change.Sources[1])), projectDiagnosticMaxSourceRunes)
	}
	after, _ := json.Marshal(s.changes)
	require.Equal(t, string(before), string(after))
	response, err := restoreProjectResponse(ctx, account, mapped)
	require.NoError(t, err)
	for _, id := range ids {
		require.Contains(t, string(response), id)
	}
	d = s.diagnostic()
	replaced, restored := d.OmittedReplaced, d.OmittedRestored
	for _, change := range d.Changes {
		replaced += change.Replaced
		restored += change.Restored
	}
	wantReplaced, wantRestored := 0, 0
	for _, change := range s.changes {
		wantReplaced += change.Replaced
		wantRestored += change.Restored
	}
	require.Equal(t, wantReplaced, replaced)
	require.Equal(t, wantRestored, restored)
}
