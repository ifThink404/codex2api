package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func unifyCredential(r *AccountRow, key string) string {
	value, _ := r.Credentials[key].(string)
	return value
}

func TestPlanExcelBPSUnify(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name                    string
		excel, optOut, override *bool
		want                    excelBPSUnifyPlan
	}{
		{"excel on", &yes, nil, nil, excelBPSUnifyPlan{set: true, enabled: true, excelLabel: true}},
		{"excel on beats a stale opt-out", &yes, &yes, nil, excelBPSUnifyPlan{set: true, enabled: true, excelLabel: true}},
		{"opt-out", &no, &yes, nil, excelBPSUnifyPlan{set: true, enabled: false}},
		{"nothing", &no, &no, nil, excelBPSUnifyPlan{}},
		{"explicit plugin off wins", &yes, nil, &no, excelBPSUnifyPlan{}},
		{"explicit plugin on keeps excel profile", &yes, nil, &yes, excelBPSUnifyPlan{excelLabel: true}},
		{"explicit plugin on beats opt-out", nil, &yes, &yes, excelBPSUnifyPlan{}},
	} {
		require.Equal(t, tc.want, planExcelBPSUnify(tc.excel, tc.optOut, tc.override), tc.name)
	}
}

func TestMigrateExcelBPSUnifyOnce(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := openTransportPluginTestDB(t, driver)
			ctx := context.Background()
			// Production databases ran the older BPS migrations long ago (the
			// legacy one would clear codex_bps_enabled=false); replay only ours.
			require.NoError(t, db.MigrateBPSPlugin(ctx))
			_, err := db.conn.ExecContext(ctx, `DELETE FROM transport_plugin_migrations WHERE plugin='bps' AND name=$1`, bpsExcelUnifyMigration)
			require.NoError(t, err)
			ids := map[string]int64{}
			for name, extra := range map[string]map[string]any{
				"excel":        {"openai_excel_bps": true},
				"excel-string": {"openai_excel_bps": "true"},
				"opt-out":      {"openai_excel_bps_opt_out": true},
				"plugin-off":   {"openai_excel_bps": true, "codex_bps_enabled": false},
				"plugin-on":    {"openai_excel_bps_opt_out": true, "codex_bps_enabled": true, "codex_bps_profile": "word"},
				"cleared":      {"openai_excel_bps": false, "openai_excel_bps_opt_out": false},
				"untouched":    {"codex_bps_enabled": true},
			} {
				creds := map[string]interface{}{"refresh_token": "rt-" + name + fmt.Sprint(time.Now().UnixNano()), "access_token": "secret-" + name}
				for k, v := range extra {
					creds[k] = v
				}
				id, err := db.InsertAccountWithCredentials(ctx, "unify-"+name, creds, "")
				require.NoError(t, err)
				ids[name] = id
			}
			seedLegacyBasispointsSettings(t, db, "")
			require.NoError(t, db.SaveTransportPluginState(ctx, TransportPluginState{ID: "bps", Config: []byte(`{"bps_min_usable_accounts":5}`)}))

			before, err := db.SchedulerOutboxHighWatermark(ctx)
			require.NoError(t, err)
			require.NoError(t, db.MigrateBPSPlugin(ctx))

			row := func(name string) *AccountRow {
				r, err := db.GetAccountByID(ctx, ids[name])
				require.NoError(t, err)
				return r
			}
			expect := func(name string, enabled *bool, profile string) {
				t.Helper()
				r := row(name)
				_, hasExcel := r.Credentials["openai_excel_bps"]
				_, hasOptOut := r.Credentials["openai_excel_bps_opt_out"]
				require.False(t, hasExcel || hasOptOut, "%s: legacy keys removed", name)
				require.Equal(t, enabled, r.GetCredentialOptionalBool("codex_bps_enabled"), name)
				require.Equal(t, profile, unifyCredential(r, "codex_bps_profile"), name)
				require.Equal(t, "secret-"+name, unifyCredential(r, "access_token"), "%s: other credentials intact", name)
			}
			yes, no := true, false
			expect("excel", &yes, "excel")
			expect("excel-string", &yes, "excel")
			expect("opt-out", &no, "")
			expect("plugin-off", &no, "")
			expect("plugin-on", &yes, "word")
			expect("cleared", nil, "")
			expect("untouched", &yes, "")

			states, err := db.ListTransportPluginStates(ctx)
			require.NoError(t, err)
			require.Len(t, states, 1)
			require.True(t, states[0].Enabled, "codex_basispoints_enabled turned the plugin's global switch on")
			require.JSONEq(t, `{"bps_min_usable_accounts":5}`, string(states[0].Config), "plugin config kept")

			events, err := db.ListSchedulerOutboxEventsAfter(ctx, before, 100)
			require.NoError(t, err)
			reloaded := map[int64]bool{}
			plugin := false
			for _, event := range events {
				if event.EntityType == SchedulerEntityAccount {
					reloaded[event.EntityID] = true
				}
				plugin = plugin || event.EntityType == SchedulerEntityPlugin
			}
			for _, name := range []string{"excel", "excel-string", "opt-out", "plugin-off", "plugin-on", "cleared"} {
				require.True(t, reloaded[ids[name]], "%s reloads on every replica", name)
			}
			require.False(t, reloaded[ids["untouched"]])
			require.True(t, plugin, "the plugin row reloads on every replica")

			// Once only: a later global switch-off and a re-added flag stay as written.
			states[0].Enabled = false
			require.NoError(t, db.SaveTransportPluginState(ctx, states[0]))
			require.NoError(t, db.UpdateCredentials(ctx, ids["untouched"], map[string]interface{}{"openai_excel_bps": true}))
			require.NoError(t, db.MigrateBPSPlugin(ctx))
			states, err = db.ListTransportPluginStates(ctx)
			require.NoError(t, err)
			require.False(t, states[0].Enabled)
			require.True(t, row("untouched").GetCredentialBool("openai_excel_bps"))
		})
	}
}

func TestMigrateExcelBPSUnifyLeavesGlobalOffAlone(t *testing.T) {
	db := openTransportPluginTestDB(t, "sqlite")
	ctx := context.Background()
	require.NoError(t, db.MigrateBPSPlugin(ctx))
	states, err := db.ListTransportPluginStates(ctx)
	require.NoError(t, err)
	require.Empty(t, states, "no plugin row is created when codex_basispoints_enabled is off")
}

// seedLegacyBasispointsSettings gives the database the shape of one created
// before upstream v3.0.6: the retired codex_basispoints_* columns exist and
// upstream's global Basispoints switch is on.
func seedLegacyBasispointsSettings(t *testing.T, db *DB, models string) {
	t.Helper()
	ctx := context.Background()
	settings, err := db.GetSystemSettings(ctx)
	require.NoError(t, err)
	if settings == nil {
		settings = &SystemSettings{}
	}
	require.NoError(t, db.UpdateSystemSettings(ctx, settings))
	for _, column := range []string{"codex_basispoints_enabled BOOLEAN DEFAULT FALSE", "codex_basispoints_models TEXT DEFAULT ''"} {
		ddl := `ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS ` + column
		if db.isSQLite() {
			ddl = `ALTER TABLE system_settings ADD COLUMN ` + column
		}
		_, err := db.conn.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	_, err = db.conn.ExecContext(ctx, `UPDATE system_settings SET codex_basispoints_enabled = $1, codex_basispoints_models = $2 WHERE id = 1`, true, models)
	require.NoError(t, err)
}
