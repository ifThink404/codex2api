package auth

const OpenAIRawPassthroughCredentialKey = "raw_passthrough_enabled"

func (a *Account) OpenAIRawPassthroughEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isOpenAIResponsesAPILocked() && a.OpenAIRawPassthrough
}

func (s *Store) ApplyOpenAIRawPassthrough(id int64, enabled bool) bool {
	a := s.FindByID(id)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.isOpenAIResponsesAPILocked() {
		return false
	}
	a.OpenAIRawPassthrough = enabled
	return true
}
