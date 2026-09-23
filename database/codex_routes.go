package database

// Preserve the effective legacy Codex switch when only BPS is edited. This is
// per-row inside the transaction, so mixed legacy batches retain their settings.
func mergeCodexRouteCredentials(current, updates map[string]interface{}) map[string]interface{} {
	base := cloneCredentialUpdates(current)
	if _, changingBPS := updates["codex_bps_enabled"]; changingBPS && current["codex_native_enabled"] == nil {
		if base == nil {
			base = make(map[string]interface{})
		}
		row := AccountRow{Credentials: current}
		base["codex_native_enabled"] = !row.GetCredentialBool("codex_bps_enabled")
	}
	return mergeCredentialMaps(base, updates)
}
