package wsrelay

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
)

func TestNativeCompactionPolicyRejectsBeforeWebsocketAcquire(t *testing.T) {
	// No connection pool or credentials are available: rejection must happen
	// before either is needed, including callers that enter the WS executor directly.
	a := &auth.Account{CodexNativeCompactionOnly: true}
	e := &Executor{}
	response, err := e.ExecuteRequestViaWebsocket(t.Context(), a, []byte(`{"client_metadata":{"x-codex-turn-metadata":{"request_kind":"compaction"}}}`), "", "", "", nil, nil, "")
	require.Nil(t, response)
	var policyErr *proxy.Error
	require.ErrorAs(t, err, &policyErr)
	require.Equal(t, "native_compaction_required", policyErr.Code)
}
