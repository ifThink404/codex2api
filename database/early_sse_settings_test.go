package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEarlySSESettingsPersistAcrossRestart(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			// Reuse the disposable-schema helper; no live application data is used.
			db, reopen := meteringTestDB(t, driver)
			ctx := context.Background()
			require.NoError(t, db.UpdateSystemSettings(ctx, &SystemSettings{
				SiteName: "early SSE", GlobalRPM: 1234, UsageMeteringEnabled: true,
				CodexPreflightSSEPassthroughEnabled: true,
			}))
			settings, err := db.GetSystemSettings(ctx)
			require.NoError(t, err)
			require.False(t, settings.CodexEarlySSEPassthroughEnabled, "upgrading must retain buffered delivery")
			for _, enabled := range []bool{true, false} {
				settings.CodexEarlySSEPassthroughEnabled = enabled
				require.NoError(t, db.UpdateSystemSettings(ctx, settings))
				require.NoError(t, db.Close())
				db = reopen()
				settings, err = db.GetSystemSettings(ctx)
				require.NoError(t, err)
				require.Equal(t, enabled, settings.CodexEarlySSEPassthroughEnabled)
				require.True(t, settings.CodexPreflightSSEPassthroughEnabled)
				require.True(t, settings.UsageMeteringEnabled)
				require.Equal(t, 1234, settings.GlobalRPM)
			}
			// A database from before this feature has neither the column nor an
			// explicit preference. Re-running startup migration must add false.
			_, err = db.conn.ExecContext(ctx, `ALTER TABLE system_settings DROP COLUMN codex_early_sse_passthrough_enabled`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			db = reopen()
			settings, err = db.GetSystemSettings(ctx)
			require.NoError(t, err)
			require.False(t, settings.CodexEarlySSEPassthroughEnabled)
			require.True(t, settings.CodexPreflightSSEPassthroughEnabled)
			require.Equal(t, 1234, settings.GlobalRPM)
		})
	}
}
