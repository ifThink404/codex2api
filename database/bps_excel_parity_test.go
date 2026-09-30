package database

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// productionExcelShape builds the production database as it is before the
// plugin: upstream's Excel Basispoints on globally, 3 accounts with the old
// local codex_bps_enabled=false, 8 without it, no native route set, and two
// accounts BPS never serves (a Responses relay and an agent identity).
func productionExcelShape(t *testing.T, db *DB, models string) (legacyOff, unset []int64, relay, agent int64) {
	t.Helper()
	ctx := context.Background()
	insert := func(name string, extra map[string]any) int64 {
		creds := map[string]any{"refresh_token": "rt-" + name + fmt.Sprint(time.Now().UnixNano()), "access_token": "at-" + name}
		for k, v := range extra {
			creds[k] = v
		}
		id, err := db.InsertAccountWithCredentials(ctx, name, creds, "")
		require.NoError(t, err)
		return id
	}
	for i := 0; i < 3; i++ {
		legacyOff = append(legacyOff, insert(fmt.Sprintf("legacy-off-%d", i), map[string]any{"codex_bps_enabled": false}))
	}
	for i := 0; i < 8; i++ {
		unset = append(unset, insert(fmt.Sprintf("oauth-%d", i), nil))
	}
	relay = insert("relay", map[string]any{"upstream_type": "openai_responses", "base_url": "https://relay.example", "api_key": "sk-relay"})
	agent = insert("agent", map[string]any{"auth_mode": "agent_identity", "agent_runtime_id": "rt", "agent_private_key": "key"})
	settings, err := db.GetSystemSettings(ctx)
	require.NoError(t, err)
	if settings == nil {
		settings = &SystemSettings{}
	}
	settings.CodexBasispointsEnabled = true
	settings.CodexBasispointsModels = models
	require.NoError(t, db.UpdateSystemSettings(ctx, settings))
	return legacyOff, unset, relay, agent
}

func TestExcelParityMigrationOnTheProductionShape(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := openTransportPluginTestDB(t, driver)
			ctx := context.Background()
			legacyOff, unset, relay, agent := productionExcelShape(t, db, "")
			before, err := db.SchedulerOutboxHighWatermark(ctx)
			require.NoError(t, err)

			require.NoError(t, db.MigrateBPSPlugin(ctx))

			row := func(id int64) *AccountRow {
				r, err := db.GetAccountByID(ctx, id)
				require.NoError(t, err)
				return r
			}
			for _, id := range append(append([]int64{}, legacyOff...), unset...) {
				r := row(id)
				require.Nil(t, r.GetCredentialOptionalBool("codex_bps_enabled"), "account %d inherits the global switch", id)
				require.Equal(t, "excel", unifyCredential(r, "codex_bps_profile"), "account %d keeps today's Excel profile", id)
				native := r.GetCredentialOptionalBool("codex_native_enabled")
				require.NotNil(t, native, "account %d", id)
				require.True(t, *native, "account %d keeps its same-account native fallback", id)
			}
			for _, id := range []int64{relay, agent} {
				r := row(id)
				require.Nil(t, r.GetCredentialOptionalBool("codex_native_enabled"), "account %d is never served by BPS", id)
				require.Empty(t, unifyCredential(r, "codex_bps_profile"))
			}

			states, err := db.ListTransportPluginStates(ctx)
			require.NoError(t, err)
			require.Len(t, states, 1)
			state := states[0]
			require.True(t, state.Enabled, "the plugin global switch follows codex_basispoints_enabled")
			require.JSONEq(t, `{"dual_route_preference":"bps"}`, string(state.Config), "BPS first; no budgets, caps or scheduled probes")
			require.False(t, state.CaptureEnabled)
			require.Zero(t, state.CaptureSampleRate)
			require.Empty(t, state.GroupIDs)

			var migrations []string
			rows, err := db.conn.QueryContext(ctx, `SELECT name FROM transport_plugin_migrations WHERE plugin='bps' ORDER BY name`)
			require.NoError(t, err)
			for rows.Next() {
				var name string
				require.NoError(t, rows.Scan(&name))
				migrations = append(migrations, name)
			}
			rows.Close()
			require.Subset(t, migrations, []string{"legacy_false_switch", "excel_bps_unify", "excel_parity"})

			var audits int
			require.NoError(t, db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_events WHERE event_type='bps_excel_parity' AND source='migration'`).Scan(&audits))
			require.Equal(t, 11, audits, "one audit row per changed account")
			events, err := db.ListSchedulerOutboxEventsAfter(ctx, before, 200)
			require.NoError(t, err)
			reloaded := map[int64]bool{}
			for _, event := range events {
				if event.EntityType == SchedulerEntityAccount {
					reloaded[event.EntityID] = true
				}
			}
			for _, id := range append(append([]int64{}, legacyOff...), unset...) {
				require.True(t, reloaded[id], "account %d reloads on every replica", id)
			}

			// Once only: a later operator change survives restarts.
			require.NoError(t, db.UpdateCredentials(ctx, unset[0], map[string]interface{}{"codex_native_enabled": false}))
			require.NoError(t, db.MigrateBPSPlugin(ctx))
			native := row(unset[0]).GetCredentialOptionalBool("codex_native_enabled")
			require.NotNil(t, native)
			require.False(t, *native)
		})
	}
}

func TestExcelParityCopiesUpstreamModelList(t *testing.T) {
	db := openTransportPluginTestDB(t, "sqlite")
	productionExcelShape(t, db, "GPT-6-Astra, gpt-5.6-sol;gpt-6-astra")
	require.NoError(t, db.MigrateBPSPlugin(context.Background()))
	states, err := db.ListTransportPluginStates(context.Background())
	require.NoError(t, err)
	var config struct {
		Models []string `json:"bps_models"`
	}
	require.NoError(t, json.Unmarshal(states[0].Config, &config))
	require.Equal(t, []string{"gpt-6-astra", "gpt-5.6-sol"}, config.Models)
}

// A database that already configured the plugin (fj-teat) is never touched
// by the parity step, even with codex_basispoints_enabled on.
func TestExcelParitySkipsAConfiguredPlugin(t *testing.T) {
	db := openTransportPluginTestDB(t, "sqlite")
	ctx := context.Background()
	_, unset, _, _ := productionExcelShape(t, db, "")
	require.NoError(t, db.SaveTransportPluginState(ctx, TransportPluginState{ID: "bps", Config: []byte(`{"bps_account_max_concurrency":4}`)}))
	require.NoError(t, db.MigrateBPSPlugin(ctx))
	r, err := db.GetAccountByID(ctx, unset[0])
	require.NoError(t, err)
	require.Nil(t, r.GetCredentialOptionalBool("codex_native_enabled"))
	states, err := db.ListTransportPluginStates(ctx)
	require.NoError(t, err)
	require.True(t, states[0].Enabled)
	require.JSONEq(t, `{"bps_account_max_concurrency":4}`, string(states[0].Config))
}
