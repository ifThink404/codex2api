package auth

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/codex2api/database"
)

// Transport plugin support (see proxy/plugins). auth cannot import the plugin
// package, so plugins register their per-account override credential keys
// here and the plugin runtime installs its snapshot reloader as a hook.

var (
	transportPluginKeysMu sync.RWMutex
	// plugin ID -> credential key holding that plugin's per-account override.
	transportPluginOverrideKeys = map[string]string{}

	transportPluginReloader atomic.Pointer[func(context.Context) error]
)

// RegisterTransportPluginOverrideKey declares that credentials[key] holds the
// tri-state per-account override for pluginID (absent/null = inherit). It must
// run before accounts are loaded; plugin registration does this at init.
func RegisterTransportPluginOverrideKey(pluginID, key string) {
	if pluginID == "" || key == "" {
		return
	}
	transportPluginKeysMu.Lock()
	transportPluginOverrideKeys[pluginID] = key
	transportPluginKeysMu.Unlock()
}

// TransportPluginOverrideKeys returns a copy of the registered plugin ID ->
// credential key map.
func TransportPluginOverrideKeys() map[string]string {
	transportPluginKeysMu.RLock()
	defer transportPluginKeysMu.RUnlock()
	out := make(map[string]string, len(transportPluginOverrideKeys))
	for id, key := range transportPluginOverrideKeys {
		out[id] = key
	}
	return out
}

// SetTransportPluginReloader installs the callback the scheduler outbox
// consumer runs for 'plugin' events. nil uninstalls it.
func SetTransportPluginReloader(fn func(context.Context) error) {
	if fn == nil {
		transportPluginReloader.Store(nil)
		return
	}
	transportPluginReloader.Store(&fn)
}

func reloadTransportPlugins(ctx context.Context) error {
	if fn := transportPluginReloader.Load(); fn != nil {
		return (*fn)(ctx)
	}
	return nil
}

func transportPluginOverridesFromRow(row *database.AccountRow) map[string]bool {
	transportPluginKeysMu.RLock()
	defer transportPluginKeysMu.RUnlock()
	var out map[string]bool
	for id, key := range transportPluginOverrideKeys {
		if value := row.GetCredentialOptionalBool(key); value != nil {
			if out == nil {
				out = make(map[string]bool, len(transportPluginOverrideKeys))
			}
			out[id] = *value
		}
	}
	return out
}

func cloneTransportPluginOverrides(src map[string]bool) map[string]bool {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]bool, len(src))
	for id, value := range src {
		out[id] = value
	}
	return out
}

// TransportPluginOverride returns the account's override for pluginID;
// ok=false means inherit (group membership, then the plugin's global switch).
func (a *Account) TransportPluginOverride(pluginID string) (enabled, ok bool) {
	if a == nil {
		return false, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	enabled, ok = a.transportPluginOverrides[pluginID]
	return enabled, ok
}

// TransportPluginOverrideIDs lists plugin IDs with an explicit override.
func (a *Account) TransportPluginOverrideIDs() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	ids := make([]string, 0, len(a.transportPluginOverrides))
	for id := range a.transportPluginOverrides {
		ids = append(ids, id)
	}
	a.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// ApplyAccountTransportPluginOverride publishes an admin override change to
// the local runtime account immediately; other replicas follow through the
// account outbox event written with the credential.
func (s *Store) ApplyAccountTransportPluginOverride(dbID int64, pluginID string, enabled *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if enabled == nil {
		delete(acc.transportPluginOverrides, pluginID)
		return true
	}
	if acc.transportPluginOverrides == nil {
		acc.transportPluginOverrides = map[string]bool{}
	}
	acc.transportPluginOverrides[pluginID] = *enabled
	return true
}

// SetTransportPluginOverride sets an override on an account that is not (yet)
// in a store, e.g. while building test accounts.
func (a *Account) SetTransportPluginOverride(pluginID string, enabled bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.transportPluginOverrides == nil {
		a.transportPluginOverrides = map[string]bool{}
	}
	a.transportPluginOverrides[pluginID] = enabled
}
