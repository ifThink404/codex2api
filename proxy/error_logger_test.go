package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"Status: 422", "Status: 429", "Field required"} {
		if !strings.Contains(text, want) {
			t.Fatalf("client error log missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "private-") || strings.Contains(text, "Status: 200") || strings.Contains(text, "Status: 400") || strings.Contains(text, "Status: 500") {
		t.Fatalf("client error log leaked input or wrong statuses:\n%s", text)
	}
	for _, file := range []string{"bad_request.log", "server_error.log"} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Fatalf("%s not written: %v", file, err)
		}
	}
}

func TestUsageLogErrorMessageExtractsStringAndValidationDetails(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"detail":"Upstream edge rejected the request"}`, "Upstream edge rejected the request"},
		{`{"detail":[{"loc":["body","input",0],"msg":"Field required","input":"private-prompt"}]}`, `["body","input",0]: Field required`},
	} {
		got := usageLogErrorMessageImpl(422, []byte(tc.body), false)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "private-") {
			t.Fatalf("usage error message for %s = %q, want %q", tc.body, got, tc.want)
		}
	}
}
