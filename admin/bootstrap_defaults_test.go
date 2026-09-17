package admin

import (
	"os"
	"regexp"
	"testing"
)

// TestFreshInstallDefaultsEnableTurnStateVault guards against the vault
// defaulting to off on a brand-new deployment. defaultBootstrapSettings()'s
// only production caller (PostBootstrap) is gated on GetSystemSettings
// returning nil, which never happens on a real fresh install: main.go's
// step 3 runs first and unconditionally persists its own
// &database.SystemSettings{...} literals (the persisted one and its
// error-fallback twin) whenever no row exists yet. Both literals must set
// CodexTurnStateVaultEnabled explicitly — it has no normalizer, so an
// omitted field silently persists as false. See admin/bootstrap.go's
// "与 main.go 中 step 3 保持一致" comment.
func TestFreshInstallDefaultsEnableTurnStateVault(t *testing.T) {
	mainSrc, err := os.ReadFile("../main.go")
	if err != nil {
		t.Fatalf("read ../main.go: %v", err)
	}
	vaultEnabledTrue := regexp.MustCompile(`CodexTurnStateVaultEnabled:\s*true`)
	matches := vaultEnabledTrue.FindAllIndex(mainSrc, -1)
	if len(matches) < 2 {
		t.Fatalf("main.go must set CodexTurnStateVaultEnabled: true in both the persisted and error-fallback settings literals, found %d occurrence(s)", len(matches))
	}

	settings := defaultBootstrapSettings()
	if !settings.CodexTurnStateVaultEnabled {
		t.Fatal("defaultBootstrapSettings().CodexTurnStateVaultEnabled = false, want true")
	}
	if settings.CodexSessionAutoLockThreshold != 3 {
		t.Fatalf("defaultBootstrapSettings().CodexSessionAutoLockThreshold = %d, want 3", settings.CodexSessionAutoLockThreshold)
	}
}
