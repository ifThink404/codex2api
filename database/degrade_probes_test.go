package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDegradeProbesStoreListAndPrune(t *testing.T) {
	db := openBPSIdentityTestDB(t)
	ctx := context.Background()
	old := &DegradeProbe{AccountID: 7, Route: "native", Model: "gpt-6-astra", Score: 150, Verdict: "degraded", Trigger: "scheduled", HTML: "<html>old</html>", CreatedAt: time.Now().Add(-8 * 24 * time.Hour)}
	require.NoError(t, db.InsertDegradeProbe(ctx, old))
	big := &DegradeProbe{AccountID: 7, Route: "native", Model: "gpt-6-astra", UpstreamModel: "gpt-6-astra", Score: 214, Bytes: 12000, Verdict: "ok", Trigger: "manual", HTML: "<html>" + strings.Repeat("x", DegradeProbeHTMLLimit) + "</html>", DurationMs: 90000}
	require.NoError(t, db.InsertDegradeProbe(ctx, big))
	require.NoError(t, db.InsertDegradeProbe(ctx, &DegradeProbe{AccountID: 7, Route: "bps", Model: "gpt-6-astra", Verdict: "invalid", Trigger: "mismatch"}))
	require.NoError(t, db.InsertDegradeProbe(ctx, &DegradeProbe{AccountID: 8, Route: "bps", Model: "gpt-6-astra", Score: 90, Verdict: "degraded", Trigger: "recovery"}))

	all, err := db.ListDegradeProbes(ctx, DegradeProbeFilter{})
	require.NoError(t, err)
	require.Len(t, all, 4)
	require.Empty(t, all[0].HTML, "lists leave the samples out")
	native, err := db.ListDegradeProbes(ctx, DegradeProbeFilter{AccountID: 7, Route: "native", Verdict: "ok"})
	require.NoError(t, err)
	require.Len(t, native, 1)
	require.Equal(t, 214, native[0].Score)

	one, err := db.GetDegradeProbe(ctx, big.ID)
	require.NoError(t, err)
	require.Len(t, one.HTML, DegradeProbeHTMLLimit, "the sample is capped at 256 KB")
	missing, err := db.GetDegradeProbe(ctx, 999999)
	require.NoError(t, err)
	require.Nil(t, missing)

	latest, err := db.LatestDegradeProbes(ctx)
	require.NoError(t, err)
	require.Len(t, latest, 3, "one per account and route")

	deleted, err := db.PruneDegradeProbes(ctx, time.Now().Add(-DegradeProbeRetention))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
}
