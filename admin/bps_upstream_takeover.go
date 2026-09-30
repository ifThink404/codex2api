package admin

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/plugins"
)

// Upstream's Excel Basispoints settings are views of the bps transport
// plugin, which owns BPS routing in this fork:
//
//   - codex_basispoints_enabled (the global default) is the plugin's global
//     switch: settings read it from the plugin and writes save the plugin;
//   - an account's openai_excel_bps / openai_excel_bps_opt_out mode is the
//     plugin's per-account override: on = forced on (Excel profile), off =
//     forced off, inherit = no override (groups / global switch);
//   - the effective flag is whether the plugin serves the account, and the
//     adapter's 403/429 pause never applies (the plugin keeps its own).

// bpsPluginGlobalEnabled is the plugin's global switch.
func bpsPluginGlobalEnabled() bool {
	return plugins.Default().State(proxy.BPSPluginID).Enabled
}

// saveBPSPluginGlobalEnabled sets the plugin's global switch.
func saveBPSPluginGlobalEnabled(ctx context.Context, enabled bool) error {
	state := plugins.Default().State(proxy.BPSPluginID)
	if state.Enabled == enabled {
		return nil
	}
	state.Enabled = enabled
	saveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := plugins.Default().Save(saveCtx, state); err != nil {
		return err
	}
	log.Printf("[bps] global switch set to %t through codex_basispoints_enabled", enabled)
	return nil
}

// bpsPluginServes reports whether the plugin serves account (the upstream
// "effective" flag).
func bpsPluginServes(account *auth.Account) bool {
	if account == nil || !account.CodexBPSEligible() || !account.IsEnabled() {
		return false
	}
	p, ok := plugins.Default().Get(proxy.BPSPluginID)
	return ok && plugins.Default().EnabledFor(p, account)
}

// excelBPSModeFlags is the upstream mode view of a row: on when the plugin
// override is on (or the legacy opt-in is set), off when it is off.
func excelBPSModeFlags(row *database.AccountRow) (enabled, optOut bool) {
	if row == nil {
		return false, false
	}
	if row.GetCredentialBool(auth.ExcelBPSCredentialKey) {
		return true, false
	}
	switch override := row.GetCredentialOptionalBool(auth.CodexBPSEnabledCredentialKey); {
	case override == nil:
		return false, false
	case *override:
		return true, false
	default:
		return false, true
	}
}

func excelBPSModeEnabled(row *database.AccountRow) bool {
	enabled, _ := excelBPSModeFlags(row)
	return enabled
}

func excelBPSModeOptOut(row *database.AccountRow) bool {
	_, optOut := excelBPSModeFlags(row)
	return optOut
}

// translateExcelBPSMode turns an upstream mode write into the plugin
// override (and the Excel profile when it is switched on). The upstream keys
// are written too, so their own readers stay consistent.
func translateExcelBPSMode(update *accountSchedulerUpdate) {
	if !update.ExcelBPSEnabled.Set && !update.ExcelBPSOptOut.Set {
		return
	}
	enabled := update.ExcelBPSEnabled.Set && update.ExcelBPSEnabled.Value
	optOut := update.ExcelBPSOptOut.Set && update.ExcelBPSOptOut.Value
	switch auth.ExcelBPSModeFor(enabled, optOut) {
	case auth.ExcelBPSModeOn:
		update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey] = true
		update.CredentialUpdates[auth.CodexBPSProfileCredentialKey] = string(auth.BPSExcel)
	case auth.ExcelBPSModeOff:
		update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey] = false
	default:
		update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey] = nil
	}
}

// clearLegacyExcelOptIn keeps the legacy openai_excel_bps opt-in (which
// forces the plugin on) from outranking an explicit plugin override that is
// not "on".
func clearLegacyExcelOptIn(update *accountSchedulerUpdate) {
	if update.ExcelBPSEnabled.Set {
		return
	}
	value, ok := update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey]
	if !ok {
		return
	}
	if on, isBool := value.(bool); isBool && on {
		return
	}
	update.CredentialUpdates[auth.ExcelBPSCredentialKey] = false
	update.CredentialUpdates[auth.ExcelBPSOptOutCredentialKey] = value != nil
}
