package auth

const CodexNativeCompactionOnlyCredentialKey = "codex_native_compaction_only"

// The policy is opt-in. It restricts compaction requests; it does not change
// the account's upstream route or a client's compaction implementation.
func (a *Account) NativeCompactionOnlyEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexNativeCompactionOnly
}

func (s *Store) ApplyAccountNativeCompactionOnly(id int64, enabled bool) bool {
	a := s.FindByID(id)
	if a == nil {
		return false
	}
	a.mu.Lock()
	a.CodexNativeCompactionOnly = enabled
	a.mu.Unlock()
	return true
}
