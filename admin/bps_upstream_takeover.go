package admin

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/plugins"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Compat layer for upstream's Excel Basispoints inputs. The bps transport
// plugin owns BPS in this fork and has one enablement input per account (the
// codex_bps_enabled override > account groups > global switch); upstream's
// inputs are still accepted and translated, never stored:
//
//   - account writes of openai_excel_bps / openai_excel_bps_opt_out become
//     the plugin override (on = forced on with the Excel profile, off =
//     forced off, inherit = cleared); the legacy keys are not written and a
//     one-time migration (database excel_bps_unify) folded existing ones in;
//   - settings codex_basispoints_enabled is the plugin's global switch,
//     codex_basispoints_models the plugin's bps_models and
//     codex_basispoints_cache_creation_as_input its cache_creation_as_input;
//   - settings codex_basispoints_403_* / _429_* are accepted and ignored:
//     they only tuned upstream's adapter, which never runs here (the plugin
//     has its own policy-block and rate-limit handling).

// bpsPluginGlobalEnabled is the plugin's global switch.
func bpsPluginGlobalEnabled() bool {
	return plugins.Default().State(proxy.BPSPluginID).Enabled
}

// saveBPSPluginGlobalEnabled sets the plugin's global switch.
func saveBPSPluginGlobalEnabled(ctx context.Context, enabled bool) error {
	state := plugins.Default().State(proxy.BPSPluginID)
	if state.Enabled == enabled {
		return nil
	}
	state.Enabled = enabled
	saveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := plugins.Default().Save(saveCtx, state); err != nil {
		return err
	}
	log.Printf("[bps] global switch set to %t through codex_basispoints_enabled", enabled)
	return nil
}

// Plugin config keys behind upstream's settings.
const (
	bpsCacheCreationConfigKey = "cache_creation_as_input"
	bpsModelsConfigKey        = "bps_models"
)

// bpsPluginCacheCreationAsInput is the plugin's cache_creation_as_input.
func bpsPluginCacheCreationAsInput() bool {
	return gjson.GetBytes(plugins.Default().State(proxy.BPSPluginID).Config, bpsCacheCreationConfigKey).Bool()
}

// saveBPSPluginCacheCreationAsInput writes cache_creation_as_input into the
// plugin config, keeping every other key; off removes the key (the default).
func saveBPSPluginCacheCreationAsInput(ctx context.Context, enabled bool) error {
	return saveBPSPluginConfigKey(ctx, bpsCacheCreationConfigKey, true, enabled)
}

// saveBPSPluginConfigKey sets (present) or removes one plugin config key,
// keeping the others, and saves only when the value changes.
func saveBPSPluginConfigKey(ctx context.Context, key string, value any, present bool) error {
	state := plugins.Default().State(proxy.BPSPluginID)
	config := []byte(state.Config)
	if len(config) == 0 || !gjson.ValidBytes(config) {
		config = []byte("{}")
	}
	var next []byte
	var err error
	if present {
		next, err = sjson.SetBytes(config, key, value)
	} else {
		next, err = sjson.DeleteBytes(config, key)
	}
	if err != nil {
		return err
	}
	if gjson.GetBytes(next, key).Raw == gjson.GetBytes(config, key).Raw {
		return nil
	}
	state.Config = next
	saveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := plugins.Default().Save(saveCtx, state); err != nil {
		return err
	}
	log.Printf("[bps] plugin config %s set to %s through upstream settings", key, gjson.GetBytes(next, key).Raw)
	return nil
}

// bpsPluginServes reports whether the plugin serves account (the upstream
// "effective" flag).
func bpsPluginServes(account *auth.Account) bool {
	if account == nil || !account.CodexBPSEligible() || !account.IsEnabled() {
		return false
	}
	p, ok := plugins.Default().Get(proxy.BPSPluginID)
	return ok && plugins.Default().EnabledFor(p, account)
}

// excelBPSModeFlags is the upstream mode view of a row, read from the plugin
// override: on when forced on, off when forced off.
func excelBPSModeFlags(row *database.AccountRow) (enabled, optOut bool) {
	if row == nil {
		return false, false
	}
	switch override := row.GetCredentialOptionalBool(auth.CodexBPSEnabledCredentialKey); {
	case override == nil:
		return false, false
	case *override:
		return true, false
	default:
		return false, true
	}
}

func excelBPSModeEnabled(row *database.AccountRow) bool {
	enabled, _ := excelBPSModeFlags(row)
	return enabled
}

func excelBPSModeOptOut(row *database.AccountRow) bool {
	_, optOut := excelBPSModeFlags(row)
	return optOut
}

// translateExcelBPSMode turns an upstream mode write into the plugin override
// (and the Excel profile when it is switched on).
func translateExcelBPSMode(update *accountSchedulerUpdate) {
	if !update.ExcelBPSEnabled.Set && !update.ExcelBPSOptOut.Set {
		return
	}
	if update.CredentialUpdates == nil {
		update.CredentialUpdates = make(map[string]interface{})
	}
	enabled := update.ExcelBPSEnabled.Set && update.ExcelBPSEnabled.Value
	optOut := update.ExcelBPSOptOut.Set && update.ExcelBPSOptOut.Value
	on, off := true, false
	switch auth.ExcelBPSModeFor(enabled, optOut) {
	case auth.ExcelBPSModeOn:
		putBPSOverride(update.CredentialUpdates, &on)
		update.CredentialUpdates[auth.CodexBPSProfileCredentialKey] = string(auth.BPSExcel)
	case auth.ExcelBPSModeOff:
		putBPSOverride(update.CredentialUpdates, &off)
	default:
		putBPSOverride(update.CredentialUpdates, nil)
	}
}

// bpsPluginModelsText is the plugin's bps_models as upstream's comma list
// (empty = the plugin default).
func bpsPluginModelsText() string {
	var models []string
	for _, item := range gjson.GetBytes(plugins.Default().State(proxy.BPSPluginID).Config, bpsModelsConfigKey).Array() {
		models = append(models, item.String())
	}
	return strings.Join(models, ",")
}

// saveBPSPluginModels writes codex_basispoints_models (already normalized to
// a comma list) as the plugin's bps_models; empty removes the key so the
// plugin default applies. Other config keys are kept.
func saveBPSPluginModels(ctx context.Context, models string) error {
	var list []string
	for _, model := range strings.Split(models, ",") {
		if model = strings.TrimSpace(model); model != "" {
			list = append(list, model)
		}
	}
	return saveBPSPluginConfigKey(ctx, bpsModelsConfigKey, list, len(list) > 0)
}
