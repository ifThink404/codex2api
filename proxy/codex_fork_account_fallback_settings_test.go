package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

func TestCodexForkAccountFallbackSettingsRuntime(test *testing.T) {
	previous := CurrentRuntimeSettings()
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
	if DefaultRuntimeSettings().CodexForkAccountFallbackEnabled || NormalizeRuntimeSettings(RuntimeSettings{}).CodexForkAccountFallbackEnabled {
		test.Fatal("session failover must default off")
	}
	for _, enabled := range []bool{true, false} {
		next := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{CodexForkAccountFallbackEnabled: enabled})
		if next.CodexForkAccountFallbackEnabled != enabled || CurrentRuntimeSettings().CodexForkAccountFallbackEnabled != enabled {
			test.Fatalf("persisted failover setting did not reach runtime: want %t", enabled)
		}
		next = UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
			current.CodexCapacityRetryEnabled = true
			return current
		})
		if next.CodexForkAccountFallbackEnabled != enabled {
			test.Fatal("unrelated runtime update changed session failover")
		}
	}
	ApplyRuntimeSettings(RuntimeSettings{CodexForkAccountFallbackEnabled: true})
	if ApplyRuntimeSettingsFromSystem(nil).CodexForkAccountFallbackEnabled || CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
		test.Fatal("missing system settings must reset session failover to off")
	}
}
