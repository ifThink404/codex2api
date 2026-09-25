package auth

import "slices"

// TagSnapshot returns labels for management and diagnostics, not failover eligibility.
func (account *Account) TagSnapshot() []string {
	account.mu.RLock()
	defer account.mu.RUnlock()
	return slices.Clone(account.Tags)
}
