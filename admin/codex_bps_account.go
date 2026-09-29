package admin

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/plugins"
)

// Account-level controls of the BPS transport plugin. The scheduler update
// accepts them next to the other account settings; each one is a credential
// key read by auth (codexBPSAccountConfig) and by the plugin override map.

type codexBPSAccountFieldsReq struct {
	Enabled      json.RawMessage `json:"codex_bps_enabled"`
	Native       json.RawMessage `json:"codex_native_enabled"`
	NativeModels json.RawMessage `json:"codex_native_models"`
	BPSModels    json.RawMessage `json:"codex_bps_models"`
	ImageTrim    json.RawMessage `json:"codex_bps_image_trim_enabled"`
	Profile      json.RawMessage `json:"codex_bps_profile"`
	Convergence  json.RawMessage `json:"codex_bps_convergence"`
}

func (r codexBPSAccountFieldsReq) empty() bool {
	return len(r.Enabled) == 0 && len(r.Native) == 0 && len(r.NativeModels) == 0 && len(r.BPSModels) == 0 &&
		len(r.ImageTrim) == 0 && len(r.Profile) == 0 && len(r.Convergence) == 0
}

// codexBPSCredentialKeys are the credential keys the BPS fields write.
var codexBPSCredentialKeys = []string{
	auth.CodexBPSEnabledCredentialKey, auth.CodexNativeEnabledCredentialKey, auth.CodexNativeModelsCredentialKey,
	auth.CodexBPSModelsCredentialKey, auth.CodexBPSImageTrimCredentialKey, auth.CodexBPSProfileCredentialKey,
	auth.CodexBPSConvergenceCredentialKey,
}

func parseRouteModelsField(raw json.RawMessage, field string) ([]string, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return []string{}, true, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, fmt.Errorf("%s 必须是字符串数组或 null", field)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	if err := auth.ValidateCodexRouteModels(out); err != nil {
		return nil, false, fmt.Errorf("%s 无效: %w", field, err)
	}
	return out, true, nil
}

// parseCodexBPSAccountFields validates the BPS fields into credential
// updates. codex_bps_enabled, codex_native_enabled and
// codex_bps_image_trim_enabled are tri-state: null clears the explicit value
// (inherit; image trim then follows the plugin's image_trim_default).
func parseCodexBPSAccountFields(req codexBPSAccountFieldsReq, updates map[string]interface{}) error {
	for _, field := range []struct {
		raw json.RawMessage
		key string
	}{{req.Enabled, auth.CodexBPSEnabledCredentialKey}, {req.Native, auth.CodexNativeEnabledCredentialKey}, {req.ImageTrim, auth.CodexBPSImageTrimCredentialKey}} {
		value, err := parseOptionalNullableBoolField(field.raw, field.key)
		if err != nil {
			return err
		}
		if value.Set {
			if value.Value == nil {
				updates[field.key] = nil
			} else {
				updates[field.key] = *value.Value
			}
		}
	}
	profile, err := parseOptionalStringField(req.Profile, auth.CodexBPSProfileCredentialKey, auth.ValidateCodexBPSProfile)
	if err != nil {
		return err
	}
	if profile.Set {
		updates[auth.CodexBPSProfileCredentialKey] = profile.Value
	}
	convergence, err := parseOptionalStringField(req.Convergence, auth.CodexBPSConvergenceCredentialKey, auth.ValidateCodexBPSConvergence)
	if err != nil {
		return err
	}
	if convergence.Set {
		updates[auth.CodexBPSConvergenceCredentialKey] = convergence.Value
	}
	for _, field := range []struct {
		raw json.RawMessage
		key string
	}{{req.NativeModels, auth.CodexNativeModelsCredentialKey}, {req.BPSModels, auth.CodexBPSModelsCredentialKey}} {
		models, set, err := parseRouteModelsField(field.raw, field.key)
		if err != nil {
			return err
		}
		if set {
			updates[field.key] = models
		}
	}
	return nil
}

func codexBPSFieldsChanged(updates map[string]interface{}) bool {
	for _, key := range codexBPSCredentialKeys {
		if _, ok := updates[key]; ok {
			return true
		}
	}
	return false
}

// validateCodexBPSAccountTarget rejects BPS fields for accounts that cannot
// use BPS (relay, Grok, Claude, Antigravity, agent identity).
func (h *Handler) validateCodexBPSAccountTarget(id int64, updates map[string]interface{}) error {
	if !codexBPSFieldsChanged(updates) || h.store == nil {
		return nil
	}
	account := h.store.FindByID(id)
	if account == nil || account.CodexBPSEligible() {
		return nil
	}
	if enabled, ok := updates[auth.CodexBPSEnabledCredentialKey].(bool); ok && !enabled && len(updates) == 1 {
		return nil
	}
	return fmt.Errorf("该账号类型不支持 BPS")
}

// applyCodexBPSAccountRuntime publishes saved BPS fields to the live account;
// other replicas follow through the account outbox event the caller writes.
func (h *Handler) applyCodexBPSAccountRuntime(id int64, updates map[string]interface{}) {
	if !codexBPSFieldsChanged(updates) || h.store == nil {
		return
	}
	if value, ok := updates[auth.CodexBPSEnabledCredentialKey]; ok {
		enabled, isBool := value.(bool)
		if isBool {
			h.store.ApplyAccountTransportPluginOverride(id, proxy.BPSPluginID, &enabled)
		} else {
			h.store.ApplyAccountTransportPluginOverride(id, proxy.BPSPluginID, nil)
		}
	}
	h.store.ApplyAccountCodexBPSCredentialUpdates(id, updates)
}

// codexBPSAccountView is the BPS part of the admin account response.
type codexBPSAccountView struct {
	// Override is the account's explicit BPS switch (nil = inherit).
	Override *bool `json:"codex_bps_enabled"`
	// Active reports whether BPS currently serves the account (override,
	// account group, global switch or the upstream Excel flag).
	Active bool `json:"codex_bps_active"`
	auth.CodexBPSAccountSettings
	Eligible bool `json:"codex_bps_eligible"`
}

func codexBPSAccountViewFromRow(row *database.AccountRow, live *auth.Account) codexBPSAccountView {
	view := codexBPSAccountView{
		Override:                row.GetCredentialOptionalBool(auth.CodexBPSEnabledCredentialKey),
		CodexBPSAccountSettings: auth.CodexBPSAccountSettingsFromRow(row),
	}
	if live != nil {
		view.Eligible = live.CodexBPSEligible()
		if p, ok := plugins.Default().Get(proxy.BPSPluginID); ok {
			view.Active = view.Eligible && plugins.Default().EnabledFor(p, live)
		}
	}
	return view
}
