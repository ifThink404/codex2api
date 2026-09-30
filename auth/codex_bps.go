package auth

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/codex2api/database"
)

// Per-account configuration owned by the BPS transport plugin (proxy
// bps_plugin.go). Whether BPS serves an account at all is the plugin's
// enablement (codex_bps_enabled override > account group > global switch);
// the keys here only shape how an enabled account is used.
const (
	// CodexBPSEnabledCredentialKey is the BPS plugin's per-account override.
	CodexBPSEnabledCredentialKey   = "codex_bps_enabled"
	CodexBPSImageTrimCredentialKey = "codex_bps_image_trim_enabled"
	CodexBPSProfileCredentialKey   = "codex_bps_profile"
	// CodexBPSConvergenceCredentialKey selects the BPS task/turn identity
	// strategy. It is independent of the native fingerprint mode.
	CodexBPSConvergenceCredentialKey = "codex_bps_convergence"
	CodexNativeEnabledCredentialKey  = "codex_native_enabled"
	CodexNativeModelsCredentialKey   = "codex_native_models"
	CodexBPSModelsCredentialKey      = "codex_bps_models"
)

// CodexBPSProfile is the BPS product surface. One value rather than four
// booleans makes contradictory profiles impossible.
type CodexBPSProfile string

const (
	BPSWord       CodexBPSProfile = "word"
	BPSExcel      CodexBPSProfile = "excel"
	BPSSheets     CodexBPSProfile = "sheets"
	BPSPowerPoint CodexBPSProfile = "powerpoint"
)

func ValidateCodexBPSProfile(value string) error {
	switch CodexBPSProfile(value) {
	case BPSWord, BPSExcel, BPSSheets, BPSPowerPoint:
		return nil
	default:
		return fmt.Errorf("codex_bps_profile must be word, excel, sheets or powerpoint")
	}
}

func (p *CodexBPSProfile) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if err := ValidateCodexBPSProfile(value); err != nil {
		return err
	}
	*p = CodexBPSProfile(value)
	return nil
}

// NormalizeCodexBPSProfile maps missing or unknown values to Word.
func NormalizeCodexBPSProfile(value string) CodexBPSProfile {
	if ValidateCodexBPSProfile(value) != nil {
		return BPSWord
	}
	return CodexBPSProfile(value)
}

// BPS convergence strategies (codex_bps_convergence).
const (
	CodexBPSConvergenceOff       = "off"
	CodexBPSConvergenceSession   = "session"
	CodexBPSConvergenceFull      = "full"
	CodexBPSConvergenceRound     = "round"
	CodexBPSConvergenceTurnRound = "turn_round"
)

func ValidateCodexBPSConvergence(value string) error {
	switch value {
	case CodexBPSConvergenceOff, CodexBPSConvergenceSession, CodexBPSConvergenceFull, CodexBPSConvergenceRound, CodexBPSConvergenceTurnRound:
		return nil
	default:
		return fmt.Errorf("codex_bps_convergence must be off, session, full, round or turn_round")
	}
}

// NormalizeCodexBPSConvergence maps missing or unknown values to off.
func NormalizeCodexBPSConvergence(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if ValidateCodexBPSConvergence(value) != nil {
		return CodexBPSConvergenceOff
	}
	return value
}

func ValidateCodexRouteModels(models []string) error {
	if len(models) > 256 {
		return fmt.Errorf("at most 256 route models")
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 128 || strings.ContainsAny(model, " \t\r\n?[]{}\\/") || strings.Contains(strings.TrimSuffix(model, "*"), "*") {
			return fmt.Errorf("invalid route model %q: use a model ID or a prefix ending in *", model)
		}
	}
	return nil
}

type codexBPSAccountConfig struct {
	Native       *bool
	NativeModels []string
	BPSModels    []string
	// ImageTrim is the explicit codex_bps_image_trim_enabled; nil follows the
	// BPS plugin's image_trim_default.
	ImageTrim   *bool
	Profile     CodexBPSProfile
	Convergence string
}

func codexBPSAccountConfigFromRow(row *database.AccountRow) codexBPSAccountConfig {
	cfg := codexBPSAccountConfig{
		Native:       row.GetCredentialOptionalBool(CodexNativeEnabledCredentialKey),
		NativeModels: row.GetCredentialStringSlice(CodexNativeModelsCredentialKey),
		BPSModels:    row.GetCredentialStringSlice(CodexBPSModelsCredentialKey),
		ImageTrim:    row.GetCredentialOptionalBool(CodexBPSImageTrimCredentialKey),
		Profile:      NormalizeCodexBPSProfile(row.GetCredential(CodexBPSProfileCredentialKey)),
		Convergence:  NormalizeCodexBPSConvergence(row.GetCredential(CodexBPSConvergenceCredentialKey)),
	}
	return cfg
}

func (c codexBPSAccountConfig) clone() codexBPSAccountConfig {
	out := c
	if c.Native != nil {
		v := *c.Native
		out.Native = &v
	}
	if c.ImageTrim != nil {
		v := *c.ImageTrim
		out.ImageTrim = &v
	}
	out.NativeModels = slices.Clone(c.NativeModels)
	out.BPSModels = slices.Clone(c.BPSModels)
	return out
}

// CodexBPSEligible reports whether the account type can use BPS at all:
// ordinary Codex OAuth/AT credentials only.
func (a *Account) CodexBPSEligible() bool {
	return a != nil && !a.IsRelayStyle() && !a.IsCodexAgentIdentity() && !a.IsClaudeOAuth()
}

// CodexNativeRouteExplicit reports whether codex_native_enabled is
// explicitly true. A BPS account uses the native transport only then.
func (a *Account) CodexNativeRouteExplicit() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.codexBPS.Native != nil && *a.codexBPS.Native
}

// CodexBPSImageTrimOverride returns the account's explicit history-trim
// setting; ok is false when the account follows the plugin default.
func (a *Account) CodexBPSImageTrimOverride() (enabled, ok bool) {
	if !a.CodexBPSEligible() {
		return false, true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.codexBPS.ImageTrim == nil {
		return false, false
	}
	return *a.codexBPS.ImageTrim, true
}

// CodexBPSImageTrimEnabled is the explicit setting, false when unset (the
// BPS plugin resolves unset accounts to its image_trim_default).
func (a *Account) CodexBPSImageTrimEnabled() bool {
	enabled, _ := a.CodexBPSImageTrimOverride()
	return enabled
}

// EffectiveCodexBPSProfile is the profile BPS uses for this account.
func (a *Account) EffectiveCodexBPSProfile() CodexBPSProfile {
	if a == nil {
		return BPSWord
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeCodexBPSProfile(string(a.codexBPS.Profile))
}

func (a *Account) CodexBPSConvergence() string {
	if a == nil {
		return CodexBPSConvergenceOff
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeCodexBPSConvergence(a.codexBPS.Convergence)
}

// CodexNativeSetting is the explicit native-route switch (nil = inherit:
// native is on unless BPS is enabled for the account).
func (a *Account) CodexNativeSetting() *bool {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.codexBPS.Native == nil {
		return nil
	}
	v := *a.codexBPS.Native
	return &v
}

func routeModelMatches(models []string, model string) bool {
	if len(models) == 0 || model == "" {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, pattern := range models {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == model || strings.HasSuffix(pattern, "*") && strings.HasPrefix(model, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

// CodexRouteAllows reports whether mode ("bps" or "native") may serve model
// on this account. bpsEnabled is the BPS plugin's enablement for the account.
// Related (background) requests inherit the root route and skip the model
// lists. Without an explicit native switch, native is on exactly when BPS is
// off, so enabling BPS moves an account to BPS.
func (a *Account) CodexRouteAllows(mode, model string, related, bpsEnabled bool) bool {
	if a == nil || a.IsRelayStyle() {
		return false
	}
	if mode == "bps" && !a.CodexBPSEligible() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if mode == "bps" {
		return bpsEnabled && (related || routeModelMatches(a.codexBPS.BPSModels, model))
	}
	if mode != "" && mode != "native" {
		return false
	}
	enabled := !bpsEnabled
	if a.codexBPS.Native != nil {
		enabled = *a.codexBPS.Native
	}
	return enabled && (related || routeModelMatches(a.codexBPS.NativeModels, model))
}

// CodexBPSAccountSettings is the admin view of the BPS keys.
type CodexBPSAccountSettings struct {
	Native       *bool           `json:"codex_native_enabled"`
	NativeModels []string        `json:"codex_native_models"`
	BPSModels    []string        `json:"codex_bps_models"`
	ImageTrim    *bool           `json:"codex_bps_image_trim_enabled"`
	Profile      CodexBPSProfile `json:"codex_bps_profile"`
	Convergence  string          `json:"codex_bps_convergence"`
}

// CodexBPSAccountSettingsFromRow reads the BPS keys from a persisted row.
func CodexBPSAccountSettingsFromRow(row *database.AccountRow) CodexBPSAccountSettings {
	cfg := codexBPSAccountConfigFromRow(row)
	return CodexBPSAccountSettings{Native: cfg.Native, NativeModels: cfg.NativeModels, BPSModels: cfg.BPSModels, ImageTrim: cfg.ImageTrim, Profile: cfg.Profile, Convergence: cfg.Convergence}
}

// CodexBPSAccountOptions sets an account's BPS configuration directly (tests
// and callers building accounts outside the database loader).
type CodexBPSAccountOptions struct {
	Native       *bool
	NativeModels []string
	BPSModels    []string
	// ImageTrim is set explicitly (tests pin it either way).
	ImageTrim   bool
	Profile     CodexBPSProfile
	Convergence string
}

// SetCodexBPSOptions replaces the account's BPS configuration.
func (a *Account) SetCodexBPSOptions(opts CodexBPSAccountOptions) *Account {
	if a == nil {
		return a
	}
	a.mu.Lock()
	imageTrim := opts.ImageTrim
	a.codexBPS = codexBPSAccountConfig{Native: opts.Native, NativeModels: opts.NativeModels, BPSModels: opts.BPSModels, ImageTrim: &imageTrim, Profile: NormalizeCodexBPSProfile(string(opts.Profile)), Convergence: NormalizeCodexBPSConvergence(opts.Convergence)}
	a.mu.Unlock()
	return a
}

// ApplyAccountCodexBPSCredentialUpdates publishes changed BPS credential keys
// (as written by the admin API) to the live account. A nil value clears it.
func (s *Store) ApplyAccountCodexBPSCredentialUpdates(dbID int64, updates map[string]any) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	row := &database.AccountRow{Credentials: updates}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if _, ok := updates[CodexNativeEnabledCredentialKey]; ok {
		acc.codexBPS.Native = row.GetCredentialOptionalBool(CodexNativeEnabledCredentialKey)
	}
	if _, ok := updates[CodexNativeModelsCredentialKey]; ok {
		acc.codexBPS.NativeModels = row.GetCredentialStringSlice(CodexNativeModelsCredentialKey)
	}
	if _, ok := updates[CodexBPSModelsCredentialKey]; ok {
		acc.codexBPS.BPSModels = row.GetCredentialStringSlice(CodexBPSModelsCredentialKey)
	}
	if _, ok := updates[CodexBPSImageTrimCredentialKey]; ok {
		acc.codexBPS.ImageTrim = row.GetCredentialOptionalBool(CodexBPSImageTrimCredentialKey)
	}
	if _, ok := updates[CodexBPSProfileCredentialKey]; ok {
		acc.codexBPS.Profile = NormalizeCodexBPSProfile(row.GetCredential(CodexBPSProfileCredentialKey))
	}
	if _, ok := updates[CodexBPSConvergenceCredentialKey]; ok {
		acc.codexBPS.Convergence = NormalizeCodexBPSConvergence(row.GetCredential(CodexBPSConvergenceCredentialKey))
	}
	return true
}
