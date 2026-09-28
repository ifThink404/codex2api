package proxy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSTaskAffinityExplicitTaskAndIdentityBoundaries(t *testing.T) {
	body := `{"model":"gpt-6-astra","metadata":{"task_id":"` + testRootSessionA + `"},"input":"hello"}`
	first := inferredSessionFixture(t, body, "alice", "device", "client/1", 8)
	state := first.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	require.Equal(t, "client_task_id", state.diagnostic.Source)
	next := inferredSessionFixture(t, strings.Replace(body, "hello", "next turn", 1), "alice", "device", "client/2", 8)
	require.Equal(t, state.seed, next.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession).seed)
	other := inferredSessionFixture(t, body, "bob", "device", "client/1", 8)
	require.NotEqual(t, state.seed, other.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession).seed)
	// A native identity always keeps precedence over task hints.
	bindInferredBPSSessionTest(first, []byte(body), requestSessionIdentity{explicitUpstreamID: testRootSessionA}, requestRootSessionIdentity{})
	require.Empty(t, first.Request.Context().Value(inferredBPSSessionKey{}).(*inferredBPSSession).seed)
}
