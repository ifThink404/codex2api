package auth

// SetRelaxedAccountGroups bypasses group membership only. Explicit account/key
// bindings, channel and plan restrictions remain part of account authorization.
// Invalidate compact routing pools on both transitions so cached group subsets
// cannot hide newly eligible accounts after the setting is enabled.
func (s *Store) SetRelaxedAccountGroups(enabled bool) {
	if s != nil && s.relaxedAccountGroups.Swap(enabled) != enabled {
		s.invalidateRoutingSchedulers()
	}
}
