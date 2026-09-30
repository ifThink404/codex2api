package proxy

import (
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
