package database

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSwitchSessionContinuityAccountBPSNative(t *testing.T) {
	for _, scenario := range []string{"same_account", "other_account", "deferred_window", "without_reset", "without_cleanup", "preserve_input", "same_route", "stale_generation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSessionAccountFailoverFixture(t)
			f.record.UpstreamMode = "bps"
			f.record.FailoverCount = 2
			state, err := json.Marshal(f.record)
			require.NoError(t, err)
			_, err = f.db.conn.ExecContext(t.Context(), "UPDATE codex_session_continuity SET state=$2 WHERE root_key=$1", f.input.RootKey, string(state))
			require.NoError(t, err)
			f.input.AccountID, f.input.ExpectedGeneration = f.input.ExpectedAccountID, 2
			f.input.UpstreamMode, f.input.ResetOutboundWindow, f.input.LossyContextRestart = "native", true, true
			f.input.WindowThreadID, f.input.WindowNumber = "thread", 47
			wantError := false
			switch scenario {
			case "other_account":
				f.input.AccountID = 2
			case "deferred_window":
				f.input.DeferOutboundWindow = true
				f.input.WindowThreadID, f.input.WindowNumber = "", 0
			case "without_reset":
				f.input.ResetOutboundWindow, wantError = false, true
			case "without_cleanup":
				f.input.LossyContextRestart, wantError = false, true
			case "preserve_input":
				f.input.PreserveRestartInput, wantError = true, true
			case "same_route":
				f.input.UpstreamMode, wantError = "bps", true
			case "stale_generation":
				f.input.ExpectedGeneration, wantError = 1, true
			}
			before := f.snapshot(t)
			record, grant, err := f.db.SwitchSessionContinuityAccount(t.Context(), f.input)
			if wantError {
				require.Error(t, err)
				require.Equal(t, before, f.snapshot(t))
				return
			}
			require.NoError(t, err)
			require.Equal(t, "native", record.UpstreamMode)
			require.Equal(t, uint64(3), record.FailoverCount)
			require.Equal(t, f.record.Number, record.Number) // Inbound accounting is untouched.
			require.Equal(t, f.input.AccountID, grant.OwnerAccountID)
			require.Equal(t, f.admissions.Windows["window-root"].ExpiresAt, grant.ExpiresAt)
			if scenario == "deferred_window" {
				require.Empty(t, record.OutboundWindows)
			} else {
				require.Equal(t, uint64(1), record.OutboundWindows["thread"].Next)
				for _, window := range record.OutboundWindows["thread"].Entries {
					require.Equal(t, uint64(0), window.Number)
				}
			}
			_, _, err = f.db.SwitchSessionContinuityAccount(t.Context(), f.input)
			require.ErrorIs(t, err, ErrSessionOwnerConflict)
		})
	}
}
