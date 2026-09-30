package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newBPSTestDB opens a database with the BPS plugin tables, which core
// migrations no longer create.
func newBPSTestDB(driver, dsn string) (*DB, error) {
	db, err := New(driver, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.MigrateBPSPlugin(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func TestMigrateBPSPluginClearsLegacyFalseSwitchOnce(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := openTransportPluginTestDB(t, driver)
			ctx := context.Background()
			ids := map[string]int64{}
			for name, value := range map[string]any{"off": false, "on": true, "unset": nil} {
				creds := map[string]interface{}{"refresh_token": "rt-" + name + fmt.Sprint(time.Now().UnixNano())}
				if value != nil {
					creds["codex_bps_enabled"] = value
				}
				id, err := db.InsertAccountWithCredentials(ctx, "bps-"+name, creds, "")
				require.NoError(t, err)
				ids[name] = id
			}
			before, err := db.SchedulerOutboxHighWatermark(ctx)
			require.NoError(t, err)
			require.NoError(t, db.MigrateBPSPlugin(ctx))
			read := func(name string) *bool {
				row, err := db.GetAccountByID(ctx, ids[name])
				require.NoError(t, err)
				return row.GetCredentialOptionalBool("codex_bps_enabled")
			}
			require.Nil(t, read("off"), "legacy false becomes inherit")
			require.NotNil(t, read("on"))
			require.True(t, *read("on"))
			require.Nil(t, read("unset"))
			events, err := db.ListSchedulerOutboxEventsAfter(ctx, before, 100)
			require.NoError(t, err)
			found := false
			for _, event := range events {
				found = found || (event.EntityType == SchedulerEntityAccount && event.EntityID == ids["off"])
			}
			require.True(t, found, "migrated account reloads on every replica")

			// A later explicit false is a real "forced off" and survives restarts.
			require.NoError(t, db.UpdateCredentials(ctx, ids["on"], map[string]interface{}{"codex_bps_enabled": false}))
			require.NoError(t, db.MigrateBPSPlugin(ctx))
			require.NotNil(t, read("on"))
			require.False(t, *read("on"))
		})
	}
}
