package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"
)

// Upstream's Excel Basispoints inputs, folded once into the BPS plugin so the
// plugin has a single enablement input (codex_bps_enabled > groups > global).
const (
	bpsEnabledCredentialKey = "codex_bps_enabled"
	bpsProfileCredentialKey = "codex_bps_profile"
	bpsExcelUnifyMigration  = "excel_bps_unify"
)

// excelBPSUnifyPlan is the plugin override one account gets from its legacy
// Excel flags; set=false keeps the current override.
type excelBPSUnifyPlan struct {
	set        bool
	enabled    bool
	excelLabel bool
}

// planExcelBPSUnify maps the legacy flags onto the plugin override:
//   - an explicit codex_bps_enabled (true or false) is the newer, plugin-side
//     decision and is kept; openai_excel_bps=true still selects the Excel
//     profile unless the override is off;
//   - otherwise openai_excel_bps=true becomes on (Excel profile), and
//     openai_excel_bps_opt_out=true becomes off.
func planExcelBPSUnify(excel, optOut, override *bool) excelBPSUnifyPlan {
	excelOn := excel != nil && *excel
	if override != nil {
		return excelBPSUnifyPlan{excelLabel: excelOn && *override}
	}
	switch {
	case excelOn:
		return excelBPSUnifyPlan{set: true, enabled: true, excelLabel: true}
	case optOut != nil && *optOut:
		return excelBPSUnifyPlan{set: true, enabled: false}
	}
	return excelBPSUnifyPlan{}
}

func parseLegacyCredentialBool(raw sql.NullString) *bool {
	if !raw.Valid {
		return nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw.String))
	if err != nil {
		return nil
	}
	return &value
}

// migrateExcelBPSUnify runs once (transport_plugin_migrations bps /
// excel_bps_unify): account openai_excel_bps / openai_excel_bps_opt_out become
// the plugin override (and Excel profile) and are removed, and a true
// system_settings.codex_basispoints_enabled turns the plugin's global switch
// on. Changed accounts and the plugin row get outbox events so every replica
// reloads them. Only these credential keys are touched.
func (db *DB) migrateExcelBPSUnify(ctx context.Context) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var done bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM transport_plugin_migrations WHERE plugin='bps' AND name=$1)`, bpsExcelUnifyMigration).Scan(&done); err != nil || done {
			return err
		}
		query := `SELECT id, credentials->>'openai_excel_bps', credentials->>'openai_excel_bps_opt_out', credentials->>'codex_bps_enabled'
			FROM accounts WHERE credentials ? 'openai_excel_bps' OR credentials ? 'openai_excel_bps_opt_out'`
		if db.isSQLite() {
			query = `SELECT id, CAST(json_extract(credentials,'$.openai_excel_bps') AS TEXT), CAST(json_extract(credentials,'$.openai_excel_bps_opt_out') AS TEXT), CAST(json_extract(credentials,'$.codex_bps_enabled') AS TEXT)
				FROM accounts WHERE json_valid(credentials) AND (json_type(credentials,'$.openai_excel_bps') IS NOT NULL OR json_type(credentials,'$.openai_excel_bps_opt_out') IS NOT NULL)`
		}
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		type candidate struct {
			id   int64
			plan excelBPSUnifyPlan
		}
		var candidates []candidate
		for rows.Next() {
			var id int64
			var excel, optOut, override sql.NullString
			if err := rows.Scan(&id, &excel, &optOut, &override); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate{id, planExcelBPSUnify(parseLegacyCredentialBool(excel), parseLegacyCredentialBool(optOut), parseLegacyCredentialBool(override))})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range candidates {
			patch := map[string]any{}
			if c.plan.set {
				patch[bpsEnabledCredentialKey] = c.plan.enabled
			}
			if c.plan.excelLabel {
				patch[bpsProfileCredentialKey] = "excel"
			}
			encoded, err := json.Marshal(patch)
			if err != nil {
				return err
			}
			update := `UPDATE accounts SET credentials = (credentials - 'openai_excel_bps' - 'openai_excel_bps_opt_out') || $2::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $1`
			if db.isSQLite() {
				update = `UPDATE accounts SET credentials = json_patch(json_remove(credentials,'$.openai_excel_bps','$.openai_excel_bps_opt_out'), $2), updated_at = CURRENT_TIMESTAMP WHERE id = $1`
			}
			if _, err := tx.ExecContext(ctx, update, c.id, string(encoded)); err != nil {
				return err
			}
			if err := insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityAccount, c.id, "updated"); err != nil {
				return err
			}
		}
		var global sql.NullBool
		if err := tx.QueryRowContext(ctx, `SELECT codex_basispoints_enabled FROM system_settings WHERE id = 1`).Scan(&global); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var configured bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM transport_plugins WHERE id='bps')`).Scan(&configured); err != nil {
			return err
		}
		if global.Valid && global.Bool && !configured {
			// Upstream's Excel Basispoints is live and the plugin was never
			// configured: take over with today's behaviour.
			if err := migrateExcelBPSParity(ctx, tx, db.isSQLite()); err != nil {
				return err
			}
		} else if global.Valid && global.Bool {
			if _, err := tx.ExecContext(ctx, `INSERT INTO transport_plugins (id, enabled, group_ids, config, capture_enabled, capture_sample_rate, updated_at)
				VALUES ('bps', $1, '[]', '{}', $2, 0, CURRENT_TIMESTAMP)
				ON CONFLICT (id) DO UPDATE SET enabled = excluded.enabled, updated_at = CURRENT_TIMESTAMP`, true, false); err != nil {
				return err
			}
			if err := insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityPlugin, 0, "updated"); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO transport_plugin_migrations(plugin,name) VALUES('bps',$1)`, bpsExcelUnifyMigration)
		return err
	})
}

const (
	bpsNativeEnabledCredentialKey = "codex_native_enabled"
	bpsExcelParityMigration       = "excel_parity"
	bpsExcelParityAccountEvent    = "bps_excel_parity"
)

var legacyBasispointsModelSeparators = regexp.MustCompile(`[,;\s]+`)

// migrateExcelBPSParity keeps upstream's live Excel Basispoints behaviour when
// the plugin takes over a database that ran it (codex_basispoints_enabled on,
// no bps plugin row yet), inside the excel_bps_unify transaction:
//   - the plugin row: global switch on, dual_route_preference=bps (BPS first,
//     the account's native route is the fallback), upstream's model list as
//     bps_models when one was set, and the degradation breaker off (no
//     automatic pelican probes); capture, budgets, concurrency caps and the
//     scheduled pelican probe stay at their off defaults;
//   - every account BPS now serves through the global switch (Codex OAuth,
//     not agent identity, no explicit plugin "off"): codex_bps_profile=excel
//     and codex_native_enabled=true unless the account already sets them.
//
// Each changed account gets an outbox event, an account_events audit row and
// a log line; the step is recorded as bps/excel_parity.
func migrateExcelBPSParity(ctx context.Context, tx *sql.Tx, sqlite bool) error {
	// The degradation breaker starts off: no automatic pelican probes until
	// an operator turns it on from the plugin page.
	config := map[string]any{"dual_route_preference": "bps", "degrade_breaker_enabled": false}
	var modelsRaw sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT codex_basispoints_models FROM system_settings WHERE id = 1`).Scan(&modelsRaw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var models []string
	seen := map[string]bool{}
	for _, model := range legacyBasispointsModelSeparators.Split(strings.ToLower(modelsRaw.String), -1) {
		if model = strings.TrimSpace(model); model != "" && !seen[model] {
			seen[model] = true
			models = append(models, model)
		}
	}
	if len(models) > 0 {
		config["bps_models"] = models
	}
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transport_plugins (id, enabled, group_ids, config, capture_enabled, capture_sample_rate, updated_at)
		VALUES ('bps', $1, '[]', $2, $3, 0, CURRENT_TIMESTAMP)`, true, string(encodedConfig), false); err != nil {
		return err
	}
	if err := insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityPlugin, 0, "updated"); err != nil {
		return err
	}
	log.Printf("[bps] excel parity: plugin global switch on, config %s (from codex_basispoints_enabled)", encodedConfig)

	query := `SELECT id, COALESCE(credentials->>'upstream_type',''), COALESCE(credentials->>'auth_mode',''), credentials->>'codex_bps_enabled', COALESCE(credentials->>'codex_bps_profile',''), credentials->>'codex_native_enabled'
		FROM accounts WHERE deleted_at IS NULL ORDER BY id`
	if sqlite {
		query = `SELECT id, COALESCE(json_extract(credentials,'$.upstream_type'),''), COALESCE(json_extract(credentials,'$.auth_mode'),''), CAST(json_extract(credentials,'$.codex_bps_enabled') AS TEXT), COALESCE(json_extract(credentials,'$.codex_bps_profile'),''), CAST(json_extract(credentials,'$.codex_native_enabled') AS TEXT)
			FROM accounts WHERE deleted_at IS NULL AND json_valid(credentials) ORDER BY id`
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	type change struct {
		id    int64
		patch map[string]any
	}
	var changes []change
	for rows.Next() {
		var id int64
		var upstreamType, authMode, profile string
		var override, native sql.NullString
		if err := rows.Scan(&id, &upstreamType, &authMode, &override, &profile, &native); err != nil {
			rows.Close()
			return err
		}
		if strings.TrimSpace(upstreamType) != "" || strings.EqualFold(strings.TrimSpace(authMode), "agent_identity") {
			continue // relay, Grok, Claude, Antigravity and agent identity accounts never use BPS
		}
		if value := parseLegacyCredentialBool(override); value != nil && !*value {
			continue // forced off: not served by BPS
		}
		patch := map[string]any{}
		if strings.TrimSpace(profile) == "" {
			patch[bpsProfileCredentialKey] = "excel"
		}
		if parseLegacyCredentialBool(native) == nil {
			patch[bpsNativeEnabledCredentialKey] = true
		}
		if len(patch) > 0 {
			changes = append(changes, change{id, patch})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	update := `UPDATE accounts SET credentials = credentials || $2::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $1`
	if sqlite {
		update = `UPDATE accounts SET credentials = json_patch(credentials, $2), updated_at = CURRENT_TIMESTAMP WHERE id = $1`
	}
	for _, c := range changes {
		encoded, err := json.Marshal(c.patch)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, update, c.id, string(encoded)); err != nil {
			return err
		}
		if err := insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityAccount, c.id, "updated"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_events (account_id, event_type, source) VALUES ($1, $2, $3)`, c.id, bpsExcelParityAccountEvent, "migration"); err != nil {
			return err
		}
		log.Printf("[bps] excel parity: account=%d set %s (BPS via the global switch, Excel profile, same-account native fallback)", c.id, encoded)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transport_plugin_migrations(plugin,name) VALUES('bps',$1)`, bpsExcelParityMigration)
	return err
}
