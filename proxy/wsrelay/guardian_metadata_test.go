package wsrelay

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestGuardianHandshakeModeAndCurrentTrace(t *testing.T) {
	headers := http.Header{"X-Codex-Guardian": {"reviewer"}, "Traceparent": {"old-trace"}, "Tracestate": {"vendor=old"}, "Thread-Id": {"thread"}, "Session-Id": {"thread"}}
	prepareCodexHandshakeSnapshot(headers)
	require.Empty(t, headers.Get("Traceparent"))
	require.Empty(t, headers.Get("Tracestate"))
	first := websocketConnectionProfile(headers)
	require.Equal(t, "reviewer", headers.Get("X-Codex-Guardian"))
	headers.Set("X-Codex-Guardian", "classifier")
	require.NotEqual(t, first, websocketConnectionProfile(headers))
	headers.Del("X-Codex-Guardian")
	require.NotEqual(t, first, websocketConnectionProfile(headers))
}
