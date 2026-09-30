package database

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSPolicyBlockHistoryLifecycle(t *testing.T) {
	db := openBPSIdentityTestDB(t)
	ctx := context.Background()
	start := time.Now().Add(-5 * time.Hour).Truncate(time.Second)

	require.NoError(t, db.OpenBPSPolicyBlock(ctx, 7, start, 1))
	require.NoError(t, db.OpenBPSPolicyBlock(ctx, 7, start.Add(time.Hour), 2), "an escalation raises the tier of the active block")
	require.NoError(t, db.RecordBPSPolicyProbe(ctx, 7, start.Add(2*time.Hour), "blocked", 3))
	require.NoError(t, db.RecordBPSPolicyProbe(ctx, 7, start.Add(3*time.Hour), "error: bps_rate_limited", 3))
	active, history, err := db.ListBPSPolicyBlocks(ctx, 10)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Empty(t, history)
	block := active[0]
	require.Equal(t, start.Unix(), block.BlockedAt.Unix(), "blocked_at stays the first strike")
	require.Equal(t, 3, block.Tier)
	require.Equal(t, 2, block.ProbeCount)
	require.Equal(t, "error: bps_rate_limited", block.LastProbeResult)
	require.Nil(t, block.ClearedAt)
	require.InDelta(t, (5 * time.Hour).Seconds(), block.Elapsed(time.Now()).Seconds(), 2, "elapsed runs until now while active")

	cleared := start.Add(4*time.Hour + 30*time.Minute)
	duration, found, err := db.ClearBPSPolicyBlock(ctx, 7, cleared)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 4*time.Hour+30*time.Minute, duration, "the recovery time")
	_, found, err = db.ClearBPSPolicyBlock(ctx, 7, cleared)
	require.NoError(t, err)
	require.False(t, found, "nothing left to clear")

	// A second block of the same account is a new row.
	require.NoError(t, db.OpenBPSPolicyBlock(ctx, 7, time.Now().Add(-time.Hour), 1))
	require.NoError(t, db.OpenBPSPolicyBlock(ctx, 8, time.Now().Add(-2*time.Hour), 1))
	active, history, err = db.ListBPSPolicyBlocks(ctx, 10)
	require.NoError(t, err)
	require.Len(t, active, 2)
	require.Len(t, history, 1)
	require.EqualValues(t, (4*time.Hour + 30*time.Minute).Seconds(), history[0].DurationSeconds)
	require.Equal(t, 4*time.Hour+30*time.Minute, history[0].Elapsed(time.Now()), "a cleared block's elapsed is its duration")

	totals, err := db.BPSPolicyBlockTotalsAt(ctx, time.Now())
	require.NoError(t, err)
	require.Len(t, totals, 2)
	require.Equal(t, int64(7), totals[0].AccountID)
	require.Equal(t, 2, totals[0].TimesBlocked)
	require.Equal(t, 1, totals[0].Recovered)
	require.InDelta(t, (5*time.Hour + 30*time.Minute).Seconds(), float64(totals[0].TotalBlockedSeconds), 2, "cleared duration + active elapsed")
	require.EqualValues(t, (4*time.Hour + 30*time.Minute).Seconds(), totals[0].LongestBlockSeconds)
	require.Equal(t, 0, totals[1].Recovered)

	// Only cleared blocks older than the retention are pruned.
	deleted, err := db.PruneBPSPolicyBlocks(ctx, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	active, history, err = db.ListBPSPolicyBlocks(ctx, 10)
	require.NoError(t, err)
	require.Len(t, active, 2)
	require.Empty(t, history)
}
