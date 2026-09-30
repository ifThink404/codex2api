package auth

import "strings"

// Upstream's Excel Basispoints inputs. The BPS transport plugin owns BPS in
// this fork: these keys are accepted by the admin compat layer and translated
// into the plugin override (codex_bps_enabled); they are never stored, and a
// one-time migration (database excel_bps_unify) folded existing values in.
const (
	ExcelBPSCredentialKey       = "openai_excel_bps"
	ExcelBPSOptOutCredentialKey = "openai_excel_bps_opt_out"
)

// Account-level Basispoints modes of upstream's API.
const (
	ExcelBPSModeInherit = "inherit"
	ExcelBPSModeOn      = "on"
	ExcelBPSModeOff     = "off"
)

// ExcelBPSModeFor maps upstream's two flags to its account mode. An explicit
// opt-in wins over a stale opt-out so older writers keep their meaning.
func ExcelBPSModeFor(enabled, optOut bool) string {
	switch {
	case enabled:
		return ExcelBPSModeOn
	case optOut:
		return ExcelBPSModeOff
	default:
		return ExcelBPSModeInherit
	}
}

// IsExcelBPSEnabled and IsExcelBPSAvailableForModel are read only by the dead
// upstream adapter (proxy/openai_excel_bps*.go, unreachable behind its closed
// gate). They follow the in-memory ExcelBPSEnabled flag, which the migrated
// credential no longer sets. Delete them together with the adapter.
func (a *Account) IsExcelBPSEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExcelBPSEnabled && !a.isRelayStyleLocked() &&
		!a.isCodexAgentIdentityLocked() &&
		strings.TrimSpace(a.APIKey) == "" &&
		strings.TrimSpace(a.AccessToken) != ""
}

func (a *Account) IsExcelBPSAvailableForModel(model string) bool {
	return a.IsExcelBPSEnabled() && a.SupportsCodexModel(model)
}
