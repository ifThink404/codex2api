package auth

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/codex2api/database"
)

func TestTransportPluginOutboxHotReload(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "plugin-outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	const pluginID, key = "outboxplug", "transport_plugin_outboxplug_enabled"
	RegisterTransportPluginOverrideKey(pluginID, key)
	var reloads atomic.Int32
	SetTransportPluginReloader(func(context.Context) error {
		reloads.Add(1)
		return nil
	})
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
		SetTransportPluginReloader(nil)
		transportPluginKeysMu.Lock()
		delete(transportPluginOverrideKeys, pluginID)
		transportPluginKeysMu.Unlock()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	if err := db.SaveTransportPluginState(ctx, database.TransportPluginState{ID: pluginID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	waitForSchedulerProjection(t, func() bool { return reloads.Load() > 0 })

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "override-account", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://override.example",
		"api_key":       "sk-override",
		"models":        []string{"gpt-5.6"},
		key:             true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForSchedulerProjection(t, func() bool {
		enabled, ok := store.FindByID(accountID).TransportPluginOverride(pluginID)
		return ok && enabled
	})

	// Another replica's admin write: only the credential and the explicit
	// account event reach this store, never ApplyAccountTransportPluginOverride.
	off := false
	if err := db.SetAccountTransportPluginOverride(ctx, accountID, key, &off); err != nil {
		t.Fatal(err)
	}
	waitForSchedulerProjection(t, func() bool {
		enabled, ok := store.FindByID(accountID).TransportPluginOverride(pluginID)
		return ok && !enabled
	})
	if err := db.SetAccountTransportPluginOverride(ctx, accountID, key, nil); err != nil {
		t.Fatal(err)
	}
	waitForSchedulerProjection(t, func() bool {
		_, ok := store.FindByID(accountID).TransportPluginOverride(pluginID)
		return !ok
	})

	on := true
	if !store.ApplyAccountTransportPluginOverride(accountID, pluginID, &on) {
		t.Fatal("local apply failed")
	}
	if ids := store.FindByID(accountID).TransportPluginOverrideIDs(); len(ids) != 1 || ids[0] != pluginID {
		t.Fatalf("override ids = %v", ids)
	}
}
