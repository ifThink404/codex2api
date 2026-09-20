package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestProjectControlMetadataRetainedAndConverged(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	for _, carrier := range []string{"x-codex-turn-metadata", "x_codex_turn_metadata"} {
		for _, encoded := range []bool{false, true} {
			t.Run(carrier+"/"+map[bool]string{true: "string", false: "object"}[encoded], func(t *testing.T) {
				canonical := map[string]any{"projectId": projectTestID, "workspace_id": projectTestID, "unknown": map[string]string{"account_id": "private"}}
				var embedded any = canonical
				if encoded {
					raw, _ := json.Marshal(canonical)
					embedded = string(raw)
				}
				body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": projectTestID, "project_id": projectTestID,
					"client_metadata": map[string]any{"project_id": unrelatedProjectUUID, "projectId": unrelatedProjectUUID, "workspaceId": projectTestID, carrier: embedded, "unknown": map[string]string{"session_id": "private"}}})
				headers := http.Header{"x-codex-project-id": []string{unrelatedProjectUUID}, "X-Codex-Workspace-Id": []string{projectTestID}, "X-Codex-Turn-Metadata": []string{`{"project_id":"` + unrelatedProjectUUID + `"}`}}
				originalBody, originalHeaders := string(body), headers.Clone()
				c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
				ctx, mapped, err := PrepareCodexProjectOutbound(c.Request.Context(), account, body, headers)
				require.NoError(t, err)
				mapped, cleanHeaders := PrepareCodexOutboundMetadata(account, mapped, headers)
				// Simulate late account custom headers after all input was sanitized.
				cleanHeaders.Set("X-Codex-Project-Id", "operator-raw")
				cleanHeaders.Set("X-Codex-Workspace-Id", "operator-raw")
				mapped, cleanHeaders = FinalizeCodexOutboundMetadata(mapped, cleanHeaders, ctx)
				alias := projectIdentityFrom(ctx).forward[projectTestID]
				require.NotEmpty(t, alias)
				require.NoError(t, ValidateCodexOutboundMetadata(mapped, cleanHeaders))
				require.Equal(t, alias, gjson.GetBytes(mapped, "input").String())
				for _, field := range []string{"project_id", "projectId", "workspace_id", "workspaceId"} {
					require.Equal(t, alias, gjson.GetBytes(mapped, "client_metadata."+field).String(), field)
				}
				for _, field := range []string{"project_id", "workspace_id"} {
					require.Equal(t, alias, gjson.Get(gjson.GetBytes(mapped, "client_metadata.x-codex-turn-metadata").String(), field).String())
					require.Equal(t, alias, gjson.Get(cleanHeaders.Get(codexTurnMetadataHeader), field).String())
					require.Equal(t, alias, cleanHeaders.Get(projectControlHeaders[field]))
				}
				require.NotContains(t, string(mapped), projectTestID)
				require.NotContains(t, string(mapped), "private")
				require.False(t, gjson.GetBytes(mapped, "project_id").Exists())
				require.NotContains(t, cleanHeaders, "x-codex-project-id")
				again, againHeaders := FinalizeCodexOutboundMetadata(mapped, cleanHeaders, ctx)
				require.JSONEq(t, string(mapped), string(again))
				require.Equal(t, cleanHeaders, againHeaders)
				require.Equal(t, originalBody, string(body))
				require.Equal(t, originalHeaders, headers)
				bad := cleanHeaders.Clone()
				bad.Set("X-Codex-Project-Id", projectTestID)
				require.Error(t, ValidateCodexOutboundMetadata(mapped, bad))
			})
		}
	}
}

func TestProjectControlMetadataSourcesAndInvalidValues(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	for _, tc := range []struct {
		name, body string
		headers    http.Header
		want       string
	}{
		{"metadata-only", `{"client_metadata":{"project_id":"` + projectTestID + `"}}`, nil, "project_id"},
		{"workspace-only", `{"input":"` + projectTestID + `","client_metadata":{"workspace_id":"` + projectTestID + `"}}`, nil, "workspace_id"},
		{"header-only", `{"input":"` + projectTestID + `"}`, http.Header{"x-codex-project-id": []string{projectTestID}}, "project_id"},
		{"workspace-header-only", `{"input":"` + projectTestID + `"}`, http.Header{"x-codex-workspace-id": []string{projectTestID}}, "workspace_id"},
		{"ws-header-only", `{"type":"response.create","input":"` + projectTestID + `"}`, http.Header{"x-codex-project-id": []string{projectTestID}}, "project_id"},
		{"invalid", `{"client_metadata":{"project_id":{"id":"raw"},"workspace_id":"not-a-uuid"}}`, nil, ""},
		{"fresh-frame", `{"client_metadata":{"x-codex-turn-metadata":{}}}`, http.Header{"X-Codex-Project-Id": []string{projectTestID}}, ""},
		{"null-frame", `{"client_metadata":{"project_id":"` + projectTestID + `","x-codex-turn-metadata":{"project_id":null}}}`, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
			ctx, body, err := PrepareCodexProjectOutbound(c.Request.Context(), account, []byte(tc.body), tc.headers)
			require.NoError(t, err)
			body, headers := PrepareCodexOutboundMetadata(account, body, tc.headers)
			body, headers = FinalizeCodexOutboundMetadata(body, headers, ctx)
			require.NoError(t, ValidateCodexOutboundMetadata(body, headers))
			if tc.want == "" {
				requireNoCodexProjectMetadata(t, body, headers)
				return
			}
			alias := projectIdentityFrom(ctx).forward[projectTestID]
			require.NotEmpty(t, alias)
			require.Equal(t, alias, gjson.Get(headers.Get(codexTurnMetadataHeader), tc.want).String())
			if gjson.Get(tc.body, "client_metadata").Exists() || gjson.Get(tc.body, "type").String() == "response.create" {
				require.Equal(t, alias, gjson.GetBytes(body, "client_metadata."+tc.want).String())
			} else {
				require.False(t, gjson.GetBytes(body, "client_metadata").Exists())
				require.Equal(t, alias, headers.Get(projectControlHeaders[tc.want]))
			}
			require.Positive(t, projectIdentityFrom(ctx).changes[projectTestID].Replaced)
		})
	}
	for _, body := range []string{
		`{"client_metadata":{"project_id":"` + projectTestID + `","projectId":"` + unrelatedProjectUUID + `"}}`,
		`{"client_metadata":{"project_id":"` + projectTestID + `","\u0070roject_id":"` + unrelatedProjectUUID + `"}}`,
	} {
		c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
		_, _, err := PrepareCodexProjectOutbound(c.Request.Context(), account, []byte(body), nil)
		require.Error(t, err)
	}
}
