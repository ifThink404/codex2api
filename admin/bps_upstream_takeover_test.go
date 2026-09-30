package admin

import (
	"encoding/json"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

// Upstream's Basispoints account mode writes the BPS plugin override.
func TestExcelBPSModeIsThePluginOverride(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, optOut string
		want            any
	}{
		{"on", `true`, `false`, true},
		{"off", `false`, `true`, false},
		{"inherit", `false`, `false`, nil},
	} {
		update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{ExcelBPSEnabled: json.RawMessage(tc.enabled), ExcelBPSOptOut: json.RawMessage(tc.optOut)})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey]
		if !ok || got != tc.want {
			t.Fatalf("%s: codex_bps_enabled = %#v (set %v), want %#v", tc.name, got, ok, tc.want)
		}
		if profile, set := update.CredentialUpdates[auth.CodexBPSProfileCredentialKey]; (tc.name == "on") != set || set && profile != string(auth.BPSExcel) {
			t.Fatalf("%s: profile = %#v", tc.name, profile)
		}
	}

	// A plugin override that is not "on" clears the legacy Excel opt-in,
	// which would otherwise force the plugin on.
	update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{codexBPSAccountFieldsReq: codexBPSAccountFieldsReq{Enabled: json.RawMessage(`false`)}})
	if err != nil {
		t.Fatal(err)
	}
	if update.CredentialUpdates[auth.ExcelBPSCredentialKey] != false || update.CredentialUpdates[auth.ExcelBPSOptOutCredentialKey] != true {
		t.Fatalf("plugin off: %#v", update.CredentialUpdates)
	}
	update, _ = parseAccountSchedulerUpdate(updateAccountSchedulerReq{codexBPSAccountFieldsReq: codexBPSAccountFieldsReq{Enabled: json.RawMessage(`true`)}})
	if _, touched := update.CredentialUpdates[auth.ExcelBPSCredentialKey]; touched {
		t.Fatal("plugin on keeps the legacy opt-in untouched")
	}
}

func TestExcelBPSModeViewFollowsThePluginOverride(t *testing.T) {
	row := func(creds map[string]any) *database.AccountRow {
		return &database.AccountRow{Credentials: creds}
	}
	for _, tc := range []struct {
		creds           map[string]any
		enabled, optOut bool
	}{
		{map[string]any{}, false, false},
		{map[string]any{auth.CodexBPSEnabledCredentialKey: true}, true, false},
		{map[string]any{auth.CodexBPSEnabledCredentialKey: false}, false, true},
		{map[string]any{auth.ExcelBPSCredentialKey: true}, true, false},
	} {
		enabled, optOut := excelBPSModeFlags(row(tc.creds))
		if enabled != tc.enabled || optOut != tc.optOut {
			t.Fatalf("%v: mode = %v/%v", tc.creds, enabled, optOut)
		}
	}
	if bpsPluginServes(nil) {
		t.Fatal("nil account is never served")
	}
	on := true
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 5, AccessToken: "at", Status: auth.StatusReady})
	store.ApplyAccountTransportPluginOverride(5, proxy.BPSPluginID, &on)
	if !bpsPluginServes(store.FindByID(5)) {
		t.Fatal("the effective flag is the plugin serving the account")
	}
}
