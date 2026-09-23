package auth

import (
	"fmt"
	"strings"

	"github.com/codex2api/database"
)

const (
	CodexNativeEnabledCredentialKey = "codex_native_enabled"
	CodexNativeModelsCredentialKey  = "codex_native_models"
	CodexBPSModelsCredentialKey     = "codex_bps_models"
)

// Nil preserves the old mutually-exclusive configuration until explicitly saved.
func CodexNativeEnabledFromRow(row *database.AccountRow) *bool {
	if row == nil || row.Credentials[CodexNativeEnabledCredentialKey] == nil {
		return nil
	}
	v := row.GetCredentialBool(CodexNativeEnabledCredentialKey)
	return &v
}

func CodexNativeEnabledValue(row *database.AccountRow) bool {
	if v := CodexNativeEnabledFromRow(row); v != nil {
		return *v
	}
	return row == nil || !row.GetCredentialBool(CodexBPSEnabledCredentialKey)
}

func ValidateCodexRouteModels(models []string) error {
	if len(models) > 256 {
		return fmt.Errorf("路径模型最多配置 256 项")
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 128 || strings.ContainsAny(model, " \t\r\n?[]{}\\/") || strings.Contains(strings.TrimSuffix(model, "*"), "*") {
			return fmt.Errorf("无效的路径模型 %q：请填写模型 ID 或末尾带 * 的前缀", model)
		}
	}
	return nil
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

// Route lists select main turns. Related background requests inherit the root
// route, while upstream permissions still apply to their own model.
func (a *Account) CodexRouteAllows(mode, model string, related bool) bool {
	if a == nil || a.IsRelayStyle() {
		return false
	}
	if mode == "bps" && a.IsCodexAgentIdentity() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if mode == "bps" {
		return a.CodexBPS && (related || routeModelMatches(a.CodexBPSModels, model))
	}
	if mode != "" && mode != "native" {
		return false
	}
	enabled := !a.CodexBPS
	if a.CodexNative != nil {
		enabled = *a.CodexNative
	}
	return enabled && (related || routeModelMatches(a.CodexNativeModels, model))
}

func (s *Store) ApplyAccountCodexRoutes(id int64, native, bps database.OptionalBool, nativeModels, bpsModels []string, setNativeModels, setBPSModels bool) {
	a := s.FindByID(id)
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if bps.Set {
		if a.CodexNative == nil {
			v := !a.CodexBPS
			a.CodexNative = &v
		}
		a.CodexBPS = bps.Value
	}
	if native.Set {
		v := native.Value
		a.CodexNative = &v
	}
	if setNativeModels {
		a.CodexNativeModels = append([]string(nil), nativeModels...)
	}
	if setBPSModels {
		a.CodexBPSModels = append([]string(nil), bpsModels...)
	}
}
