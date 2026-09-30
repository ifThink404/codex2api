package proxy

import (
	"strings"
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// excelBPSAdapterEnabled opens upstream's Excel Basispoints adapter; it stays
// off outside upstream's adapter tests (see excelBPSRouteAvailable).
var excelBPSAdapterEnabled atomic.Bool

// excelBPSRouteAvailable is the single account/model gate for the Excel
// Basispoints adapter. It combines the effective account setting (explicit
// opt-in, or the global default without an opt-out), the account's Codex model
// allowlist and the optional global Basispoints model list. Request-shaped
// reasons to stay on native Codex are decided later by handleExcelBPS.
//
// In this fork the bps transport plugin owns BPS routing (Chat, Messages,
// Responses and Responses WebSocket all resolve it through
// resolveTransportPlugin), so this gate never opens in production and none of
// the adapter's native fallbacks (401, 5xx, transport, model access,
// encrypted context) run. Only upstream's own adapter tests open it.
func excelBPSRouteAvailable(account *auth.Account, model string) bool {
	if !excelBPSAdapterEnabled.Load() {
		return false
	}
	if account == nil || !account.IsExcelBPSAvailableForModel(model) {
		return false
	}
	if !excelBPSModelListed(CurrentRuntimeSettings().CodexBasispointsModels, model) {
		return false
	}
	// A cooling or paused Basispoints route goes straight to native Codex.
	return !excelBPSHealth.blocks(account, model)
}

// excelBPSModelListed reports whether model passes the normalized global list.
// An empty list does not restrict models.
func excelBPSModelListed(list, model string) bool {
	models := database.ParseCodexBasispointsModels(list)
	if len(models) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, listed := range models {
		if listed == model {
			return true
		}
	}
	return false
}
