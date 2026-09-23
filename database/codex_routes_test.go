package database

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSwitchSessionRouteSameAccountAndNoReverse(t *testing.T) {
	f := newSessionAccountFailoverFixture(t)
	f.input.AccountID, f.input.UpstreamMode = f.input.ExpectedAccountID, "bps"
	record, grant, err := f.db.SwitchSessionContinuityAccount(t.Context(), f.input)
	require.NoError(t, err)
	require.Equal(t, "bps", record.UpstreamMode)
	require.Equal(t, uint64(1), record.FailoverCount)
	require.Equal(t, f.input.AccountID, grant.OwnerAccountID)
	_, _, err = f.db.SwitchSessionContinuityAccount(t.Context(), f.input)
	require.ErrorIs(t, err, ErrSessionOwnerConflict) // Stale request cannot switch again.
	before := f.snapshot(t)
	f.input.ExpectedGeneration, f.input.AccountID, f.input.UpstreamMode = 1, 2, "native"
	_, _, err = f.db.SwitchSessionContinuityAccount(t.Context(), f.input)
	require.Error(t, err)
	require.Equal(t, before, f.snapshot(t))
	f.input.UpstreamMode = "bps"
	record, _, err = f.db.SwitchSessionContinuityAccount(t.Context(), f.input)
	require.NoError(t, err)
	require.Equal(t, "bps", record.UpstreamMode)
	require.Equal(t, uint64(2), record.FailoverCount)
}
