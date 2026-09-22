package auth

const CodexBPSEnabledCredentialKey = "codex_bps_enabled"
const CodexBPSImageTrimCredentialKey = "codex_bps_image_trim_enabled"

// Image trimming is opt-in and applies only at the BPS outbound boundary.
func (a *Account) CodexBPSImageTrimEnabled() bool {
	if a == nil || a.IsRelayStyle() || a.IsCodexAgentIdentity() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexBPSImageTrim
}

func (s *Store) ApplyAccountCodexBPSImageTrim(id int64, enabled bool) bool {
	a := s.FindByID(id)
	if a == nil {
		return false
	}
	a.mu.Lock()
	a.CodexBPSImageTrim = enabled
	a.mu.Unlock()
	return true
}

// BPS is an alternate transport for ordinary Codex credentials, not a new
// account type. Agent assertions and relay API keys are not accepted by it.
func (a *Account) CodexBPSEnabled() bool {
	if a == nil || a.IsRelayStyle() || a.IsCodexAgentIdentity() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexBPS
}

func (s *Store) ApplyAccountCodexBPS(id int64, enabled bool) bool {
	a := s.FindByID(id)
	if a == nil {
		return false
	}
	a.mu.Lock()
	a.CodexBPS = enabled
	a.mu.Unlock()
	return true
}
