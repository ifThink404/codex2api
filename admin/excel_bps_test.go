package admin

import (
	"encoding/json"
	"testing"

	"github.com/codex2api/auth"
)

func TestParseAccountSchedulerUpdateExcelBPS(t *testing.T) {
	update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{ExcelBPSEnabled: json.RawMessage(`true`)})
	if err != nil {
		t.Fatalf("parseAccountSchedulerUpdate: %v", err)
	}
	if !update.ExcelBPSEnabled.Set || !update.ExcelBPSEnabled.Value {
		t.Fatalf("ExcelBPSEnabled = %#v", update.ExcelBPSEnabled)
	}
	// Accepted as the plugin override with the Excel profile, never stored.
	if value, ok := update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey].(bool); !ok || !value || update.CredentialUpdates[auth.CodexBPSProfileCredentialKey] != string(auth.BPSExcel) {
		t.Fatalf("credential update = %#v", update.CredentialUpdates)
	}
	if _, written := update.CredentialUpdates[auth.ExcelBPSCredentialKey]; written {
		t.Fatalf("legacy key stored: %#v", update.CredentialUpdates)
	}
	if !update.hasChanges() {
		t.Fatal("BPS capability update was not recognized as a change")
	}
}

func TestParseAccountSchedulerUpdateExcelBPSRejectsNonBoolean(t *testing.T) {
	if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{ExcelBPSEnabled: json.RawMessage(`"yes"`)}); err == nil {
		t.Fatal("non-boolean BPS capability was accepted")
	}
}
