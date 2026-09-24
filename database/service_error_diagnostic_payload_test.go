package database

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceErrorDiagnosticPayloadsAreBoundedAndDetached(t *testing.T) {
	dispatch := json.RawMessage(`{"pinned_account_id":3}`)
	tools := json.RawMessage(`{"missing_names":1}`)
	event := normalizeServiceError(ServiceErrorEvent{DispatchSelection: dispatch, ToolProtocol: tools})
	dispatch[0], tools[0] = 'X', 'X'
	require.JSONEq(t, `{"pinned_account_id":3}`, string(event.DispatchSelection))
	require.JSONEq(t, `{"missing_names":1}`, string(event.ToolProtocol))
	event = normalizeServiceError(ServiceErrorEvent{DispatchSelection: json.RawMessage(`invalid`), ToolProtocol: json.RawMessage(`{"data":"` + strings.Repeat("x", 17000) + `"}`)})
	require.Empty(t, event.DispatchSelection)
	require.Empty(t, event.ToolProtocol)
}
