package proxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

func TestBPSIdentityRetentionFollowsTheLongestLifetime(t *testing.T) {
	require.Equal(t, 30*24*time.Hour, bpsIdentityRetention(BPSConfig{}.normalized()), "at least 30 days")
	require.Equal(t, 30*24*time.Hour, bpsIdentityRetention(BPSConfig{RoundTaskLifetimeHours: 48, TurnTaskLifetimeHours: 24}))
	require.Equal(t, 90*24*time.Hour, bpsIdentityRetention(BPSConfig{RoundTaskLifetimeHours: 24, TurnTaskLifetimeHours: 90 * 24}), "a longer configured lifetime wins")
}

func TestBPSMaintenancePrunesUntouchedIdentity(t *testing.T) {
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "maintain.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	key := strings.Repeat("a", 64)
	_, err = db.UpdateBPSTaskAffinity(ctx, key, 0, 1)
	require.NoError(t, err)
	reg := plugins.NewRegistry()
	reg.Register(bpsPlugin{})

	reg.RunMaintenance(ctx, db, time.Now())
	affinity, err := db.ReadBPSTaskAffinity(ctx, key)
	require.NoError(t, err)
	require.EqualValues(t, 1, affinity.AccountID, "a recently touched identity is kept")

	// Seen from 31 days later, the row is untouched for longer than 30 days.
	reg.RunMaintenance(ctx, db, time.Now().Add(31*24*time.Hour))
	affinity, err = db.ReadBPSTaskAffinity(ctx, key)
	require.NoError(t, err)
	require.Zero(t, affinity.AccountID, "the untouched identity was pruned")
}
