package database

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachmentConcurrencySettingsMigrationAndRestart(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db, reopen := meteringTestDB(t, driver)
			require.NoError(t, db.UpdateSystemSettings(t.Context(), &SystemSettings{GlobalRPM: 1234, UsageMeteringEnabled: true, CodexEarlySSEPassthroughEnabled: true}))
			s, err := db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, 15, s.BPSAttachmentRequestConcurrency)
			require.Equal(t, 64, s.BPSAttachmentInstanceConcurrency)
			require.Equal(t, 15, s.ResinAccountMaxConns)
			s.BPSAttachmentRequestConcurrency, s.BPSAttachmentInstanceConcurrency, s.ResinAccountMaxConns = 12, 128, 24
			require.NoError(t, db.UpdateSystemSettings(t.Context(), s))
			require.NoError(t, db.Close())
			db = reopen()
			s, err = db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, 12, s.BPSAttachmentRequestConcurrency)
			require.Equal(t, 128, s.BPSAttachmentInstanceConcurrency)
			require.Equal(t, 24, s.ResinAccountMaxConns)
			require.Equal(t, 1234, s.GlobalRPM)
			require.True(t, s.UsageMeteringEnabled)
			require.True(t, s.CodexEarlySSEPassthroughEnabled)
			for _, column := range []string{"bps_attachment_request_concurrency", "bps_attachment_instance_concurrency", "resin_account_max_conns"} {
				_, err = db.conn.ExecContext(t.Context(), "ALTER TABLE system_settings DROP COLUMN "+column)
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())
			db = reopen()
			s, err = db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, 15, s.BPSAttachmentRequestConcurrency)
			require.Equal(t, 64, s.BPSAttachmentInstanceConcurrency)
			require.Equal(t, 15, s.ResinAccountMaxConns)
			require.Equal(t, 1234, s.GlobalRPM)
			_, err = db.conn.ExecContext(t.Context(), `UPDATE system_settings SET bps_attachment_request_concurrency=NULL, bps_attachment_instance_concurrency=0, resin_account_max_conns=-1 WHERE id=1`)
			require.NoError(t, err)
			s, err = db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, 15, s.BPSAttachmentRequestConcurrency)
			require.Equal(t, 64, s.BPSAttachmentInstanceConcurrency)
			require.Equal(t, 15, s.ResinAccountMaxConns)
		})
	}
}
