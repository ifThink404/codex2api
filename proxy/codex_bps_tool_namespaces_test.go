package proxy

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSRestoresUniqueNamespacesAcrossProfiles(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"list_agents"}]},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},{"type":"function_call","id":"fc1","call_id":"call1","name":"list_agents","arguments":"{\"namespace\":\"business-data\"}"},{"type":"custom_tool_call","call_id":"call2","name":"exec","input":"private command"},{"type":"function_call_output","call_id":"call1","output":"private result"}]}`)
	for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		t.Run(string(profile), func(t *testing.T) {
			wire, diagnostic, err := prepareCodexBPSBodyForProfile(body, "cache", false, false, nil, bpsProfile(profile))
			require.NoError(t, err)
			require.Equal(t, 2, diagnostic.ToolNamespaceRepair.Repaired)
			require.Equal(t, "collaboration", gjson.GetBytes(wire, "input.2.namespace").String())
			require.Equal(t, "functions", gjson.GetBytes(wire, "input.3.namespace").String())
			require.Equal(t, gjson.GetBytes(body, "input.1.arguments").Raw, gjson.GetBytes(wire, "input.2.arguments").Raw)
			require.Equal(t, gjson.GetBytes(body, "input.3").Raw, gjson.GetBytes(wire, "input.4").Raw)
			require.Equal(t, "call1", gjson.GetBytes(wire, "input.2.call_id").String())
			require.Zero(t, diagnoseToolProtocol(wire).MissingNamespaces)
			encoded, err := json.Marshal(diagnostic.ToolNamespaceRepair)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "private")
			require.NotContains(t, string(encoded), "list_agents")
			second, repair, err := repairBPSToolNamespaces(wire)
			require.NoError(t, err)
			require.Equal(t, wire, second)
			require.Zero(t, repair.Repaired)
		})
	}
}

func TestBPSNamespaceRepairDoesNotGuessOrOverwrite(t *testing.T) {
	for _, extra := range []string{
		`{"type":"function","name":"list_agents"}`,
		`{"type":"namespace","name":"other","tools":[{"type":"function","name":"list_agents"}]}`,
	} {
		body := []byte(fmt.Sprintf(`{"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"list_agents"}]},%s],"input":[{"type":"function_call","name":"list_agents"},{"type":"function_call","name":"undeclared"},{"type":"custom_tool_call","name":"list_agents"},{"type":"function_call","name":"list_agents","namespace":"explicit"}]}`, extra))
		got, d, err := repairBPSToolNamespaces(body)
		require.NoError(t, err)
		require.Equal(t, body, got)
		require.Zero(t, d.Repaired)
		require.Equal(t, 3, d.Unresolved)
	}
}
