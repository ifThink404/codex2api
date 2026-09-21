package auth

import (
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestResponsesAPISessionCapacityLifecycle(t *testing.T) {
	store, account := newSessionCapacityTestStore(2)
	account.UpstreamType, account.BaseURL, account.APIKey = UpstreamOpenAIResponses, "http://relay.invalid", "relay-key"
	account.AccessToken = ""
	now := time.Now()
	enabled, limit, ttl := account.SessionCapacityConfig()
	require.True(t, enabled)
	require.EqualValues(t, 2, limit)
	require.Equal(t, time.Minute, ttl)
	require.True(t, store.AdmitAccountSession(account, "root-a", now))
	require.True(t, store.AdmitAccountSession(account, "root-b", now))
	require.False(t, store.AdmitAccountSession(account, "root-c", now))
	require.True(t, store.AdmitAccountSession(account, "root-a", now.Add(30*time.Second)))
	require.True(t, store.AdmitAccountSession(account, RelatedSessionAffinityKey("root-a"), now.Add(30*time.Second)))
	total, _ := store.AccountSessionSlotCounts(account.ID(), now.Add(30*time.Second))
	require.EqualValues(t, 2, total)
	// Only the idle window expires; an active root continues to own its slot.
	require.True(t, store.AdmitAccountSession(account, "root-c", now.Add(61*time.Second)))
	total, _ = store.AccountSessionSlotCounts(account.ID(), now.Add(61*time.Second))
	require.EqualValues(t, 2, total)
	require.True(t, store.ApplyAccountSessionCapacity(account.ID(), false, 2, 60))
	require.True(t, store.AdmitAccountSession(account, "unlimited", now))
	total, _ = store.AccountSessionSlotCounts(account.ID(), now)
	require.Zero(t, total)
}

func TestResponsesAPICapacityDoesNotEnableOtherProviderPolicies(t *testing.T) {
	account := &Account{UpstreamType: UpstreamGrok, BaseURL: "https://grok.invalid", APIKey: "key", SessionCapacityEnabled: true, SessionCapacityMax: 3}
	require.True(t, account.IsRelayStyle())
	require.False(t, account.SessionCapacityLimits().Enabled)
}

func TestResponsesAPICapacityRestoresAndAppliesPersistentUpdates(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	settings := &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1}
	newAccount := func() *Account {
		return &Account{DBID: 781, UpstreamType: UpstreamOpenAIResponses, BaseURL: "http://relay.invalid", APIKey: "key", Status: StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600}
	}
	first := NewStore(nil, runtimeCache, settings)
	defer first.Stop()
	account := newAccount()
	first.AddAccount(account)
	require.True(t, first.AdmitAccountSession(account, "persisted-api-root", time.Now()))
	first.SetAccountSessionOwner(account.ID(), "persisted-api-root", AccountSessionOwner{APIKeyID: 7})
	second := NewStore(nil, runtimeCache, settings)
	defer second.Stop()
	restored := newAccount()
	second.AddAccount(restored)
	require.False(t, second.AdmitAccountSession(restored, "new-root", time.Now()), "restart must restore the occupied slot")
	updated := newAccount()
	updated.SessionCapacityMax = 2
	second.applyPersistentAccountSnapshot(restored, updated, true)
	require.True(t, restored.SessionCapacityLimits().Enabled)
	require.True(t, second.AdmitAccountSession(restored, "new-root", time.Now()))
	total, _ := second.AccountSessionSlotCounts(restored.ID(), time.Now())
	require.EqualValues(t, 2, total)
	updated.SessionCapacityEnabled = false
	second.applyPersistentAccountSnapshot(restored, updated, true)
	_, found, err := runtimeCache.GetRuntime(t.Context(), accountSessionRuntimeNamespace, accountSessionRuntimeKey(restored.ID()))
	require.NoError(t, err)
	require.False(t, found)
}
