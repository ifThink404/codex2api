package database

import "strings"

const (
	SessionBalanceDefault = "default"
	SessionBalanceWindow  = "window"
	SessionBalanceSession = "session"
)

// NormalizeSessionBalanceMode preserves the old active-session preference when
// loading settings written before the three-way selector existed.
func NormalizeSessionBalanceMode(mode string, legacyEnabled bool) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case SessionBalanceDefault:
		return SessionBalanceDefault
	case SessionBalanceWindow:
		return SessionBalanceWindow
	case SessionBalanceSession:
		return SessionBalanceSession
	case "":
		if legacyEnabled {
			return SessionBalanceSession
		}
	}
	return SessionBalanceDefault
}
