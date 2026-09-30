package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

// enableExcelBPSAdapterForTest opens upstream's Excel Basispoints adapter
// gate for one test (it never opens in production: the BPS plugin owns BPS).
func enableExcelBPSAdapterForTest(t *testing.T) {
	t.Helper()
	previous := excelBPSAdapterEnabled.Load()
	excelBPSAdapterEnabled.Store(true)
	t.Cleanup(func() { excelBPSAdapterEnabled.Store(previous) })
}

func TestExcelBPSAdapterGateStaysClosed(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	account := &auth.Account{DBID: 7, AccessToken: "synthetic", ExcelBPSEnabled: true}
	if excelBPSRouteAvailable(account, "gpt-5.5") {
		t.Fatal("upstream's Excel Basispoints adapter must never route: the BPS plugin owns BPS")
	}
}

// Nothing from the upstream adapter starts at boot: no health prober (its
// configure hook) and no shared replay backing.
func TestExcelBPSAdapterHasNoStartupHooks(t *testing.T) {
	for file, hooks := range map[string][]string{
		"../main.go":          {"ConfigureExcelBPSReplay("},
		"handler.go":          {"excelBPSHealth.configure("},
		"../admin/handler.go": {"ClearAccountExcelBPSPause", "bps-pause"},
	} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, hook := range hooks {
			if strings.Contains(string(source), hook) {
				t.Fatalf("%s still wires the upstream adapter: %s", file, hook)
			}
		}
	}
}

// Every file of the dead adapter says so at the top, so nobody extends it.
func TestExcelBPSAdapterFilesAreMarkedDead(t *testing.T) {
	files, err := filepath.Glob("openai_excel_bps*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "basispoints/fork_dead_adapter.go")
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(source), "// Fork note: DEAD UPSTREAM ADAPTER.") {
			t.Fatalf("%s lacks the dead-adapter note", file)
		}
	}
}
