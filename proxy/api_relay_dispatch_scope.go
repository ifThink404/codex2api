package proxy

import (
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

const apiRelayDispatchScopeKey = "api_relay_dispatch_scope"

type apiRelayDispatchScope struct {
	owner  *auth.Account
	groups map[int64]struct{}
}

// API transports retain their protocol family after selecting an owner. Strict
// mode also freezes the owner's group cohort; relaxed mode uses Key scope.
func rememberAPIRelayDispatchScope(c *gin.Context, account *auth.Account) {
	if c == nil || account == nil || !account.IsOpenAIResponsesAPI() {
		return
	}
	if _, exists := c.Get(apiRelayDispatchScopeKey); !exists {
		c.Set(apiRelayDispatchScopeKey, &apiRelayDispatchScope{owner: account, groups: apiRelayConfiguredSwitchGroups(c, account)})
	}
}

func applyAPIRelayDispatchScope(c *gin.Context, inner auth.AccountFilter) auth.AccountFilter {
	if c == nil {
		return inner
	}
	return func(account *auth.Account) bool {
		if account == nil || inner != nil && !inner(account) {
			return false
		}
		value, _ := c.Get(apiRelayDispatchScopeKey)
		scope, _ := value.(*apiRelayDispatchScope)
		if scope == nil {
			return true
		}
		if account.ID() == scope.owner.ID() {
			return true
		}
		if !account.IsOpenAIResponsesAPI() {
			return false
		}
		if CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
			return true
		}
		current := apiRelayConfiguredSwitchGroups(c, scope.owner)
		for id := range current {
			if _, ok := scope.groups[id]; !ok {
				delete(current, id)
			}
		}
		return len(current) > 0 && account.InAnyGroup(current)
	}
}

// Strict switching stays within the source account's configured groups and
// Key authorization. Relaxed switching does not impose this extra cohort.
func apiRelayConfiguredSwitchGroups(c *gin.Context, source *auth.Account) map[int64]struct{} {
	row := apiKeyRowFromContext(c)
	if row == nil || source == nil {
		return nil
	}
	if len(row.AllowedGroupIDs) == 0 {
		return int64GroupSet(source.GroupIDSnapshot())
	}
	configured := int64GroupSet(row.AllowedGroupIDs)
	if configured == nil {
		configured = make(map[int64]struct{})
	}
	for _, id := range row.Limits.NoAffinityGroupIDs {
		if id > 0 {
			configured[id] = struct{}{}
		}
	}
	groups := make(map[int64]struct{})
	for _, id := range source.GroupIDSnapshot() {
		if _, ok := configured[id]; ok {
			groups[id] = struct{}{}
		}
	}
	return groups
}
