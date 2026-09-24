package auth

import (
	"encoding/json"
	"fmt"
)

const CodexBPSProfileCredentialKey = "codex_bps_profile"

// One value rather than four booleans makes contradictory profiles impossible.
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
		return fmt.Errorf("codex_bps_profile 必须为 word、excel、sheets 或 powerpoint")
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

// Missing values in existing credentials retain the original Word behavior.
func NormalizeCodexBPSProfile(value string) CodexBPSProfile {
	if ValidateCodexBPSProfile(value) != nil {
		return BPSWord
	}
	return CodexBPSProfile(value)
}

func (a *Account) EffectiveCodexBPSProfile() CodexBPSProfile {
	if a == nil {
		return BPSWord
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeCodexBPSProfile(string(a.CodexBPSProfile))
}

func (s *Store) ApplyAccountCodexBPSProfile(id int64, profile CodexBPSProfile) bool {
	a := s.FindByID(id)
	if a == nil {
		return false
	}
	a.mu.Lock()
	a.CodexBPSProfile = NormalizeCodexBPSProfile(string(profile))
	a.mu.Unlock()
	return true
}
