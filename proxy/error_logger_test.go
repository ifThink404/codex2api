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

func TestErrorLogRotatesBySize(t *testing.T) {
	t.Setenv("LOG_DISABLED", "false")
	dir := t.TempDir()
	t.Setenv("LOG_DIR", dir)
	t.Setenv("LOG_MAX_SIZE_MB", "1")
	t.Setenv("LOG_MAX_BACKUPS", "2")
	logger := &fileLogger{path: "server_error.log"}
	t.Cleanup(logger.close)
	body := []byte(`{"error":{"message":"` + strings.Repeat("x", 4000) + `"}}`)
	for range 1000 { // ~4.1 MB: rotates several times
		logger.writeEntry("/v1/responses", 500, "gpt-6-sol", 1, body)
	}
	base := filepath.Join(dir, "server_error.log")
	for _, name := range []string{base, base + ".1", base + ".2"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() > 1<<20 {
			t.Fatalf("%s is %d bytes, above the 1 MB cap", name, info.Size())
		}
	}
	if _, err := os.Stat(base + ".3"); !os.IsNotExist(err) {
		t.Fatalf("only LOG_MAX_BACKUPS backups are kept: %v", err)
	}
	data, err := os.ReadFile(base + ".1")
	if err != nil || !strings.HasPrefix(string(data), "========== ") {
		t.Fatalf("rotated file does not start at an entry boundary: %v", err)
	}
	if errorLogMaxBytes() != 1<<20 || errorLogBackups() != 2 {
		t.Fatal("env overrides not applied")
	}
	t.Setenv("LOG_MAX_SIZE_MB", "0")
	t.Setenv("LOG_MAX_BACKUPS", "99")
	if errorLogMaxBytes() != defaultErrorLogMaxMB<<20 || errorLogBackups() != defaultErrorLogBackups {
		t.Fatal("invalid overrides must fall back to the defaults")
	}
}
