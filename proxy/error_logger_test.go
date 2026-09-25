package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorLogDirUsesEnv(t *testing.T) {
	t.Setenv("LOG_DIR", "/tmp/codex2api-logs")
	if got, want := errorLogDir(), "/tmp/codex2api-logs"; got != want {
		t.Fatalf("errorLogDir() = %q, want %q", got, want)
	}
}

func TestUpstreamErrorFilesInclude422And429WithoutValidationInput(t *testing.T) {
	t.Setenv("LOG_DISABLED", "false")
	dir := t.TempDir()
	t.Setenv("LOG_DIR", dir)
	oldBad, oldServer, oldClient := badRequestLogger, serverErrorLogger, upstreamClientErrorLogger
	badRequestLogger = &fileLogger{path: "bad_request.log"}
	serverErrorLogger = &fileLogger{path: "server_error.log"}
	upstreamClientErrorLogger = &fileLogger{path: "upstream_client_error.log"}
	t.Cleanup(func() {
		CloseErrorLogger()
		badRequestLogger, serverErrorLogger, upstreamClientErrorLogger = oldBad, oldServer, oldClient
	})
	for _, status := range []int{200, 400, 422, 429, 500} {
		logUpstreamError("/v1/responses", status, "gpt-6-astra", 1, []byte(`{"detail":[{"loc":["body","input",0],"msg":"Field required","input":"private-prompt","ctx":{"secret":"private-context"}}]}`))
	}
	data, err := os.ReadFile(filepath.Join(dir, "upstream_client_error.log"))
	require.NoError(t, err)
	require.Contains(t, string(data), "Status: 422")
	require.Contains(t, string(data), "Status: 429")
	require.Contains(t, string(data), "Field required")
	require.NotContains(t, string(data), "private-")
	require.NotContains(t, string(data), "Status: 200")
	for _, file := range []string{"bad_request.log", "server_error.log"} {
		_, err := os.Stat(filepath.Join(dir, file))
		require.NoError(t, err)
	}
}
