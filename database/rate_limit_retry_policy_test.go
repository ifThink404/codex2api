package database

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRateLimitRetryPolicyMigrationAndRestart(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db, reopen := meteringTestDB(t, driver)
			s := &SystemSettings{TransportRetryPolicy: "sticky", MaxRateLimitRetries: 4, BPSAttachmentAccountConcurrency: 21}
			require.NoError(t, db.UpdateSystemSettings(t.Context(), s))
			_, err := db.conn.ExecContext(t.Context(), `ALTER TABLE system_settings DROP COLUMN rate_limit_retry_policy`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			db = reopen()
			s, err = db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, "sticky", s.RateLimitRetryPolicy, "old transport choice seeds the new independent field")
			_, err = db.conn.ExecContext(t.Context(), `UPDATE system_settings SET transport_retry_policy='rotate' WHERE id=1`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			db = reopen()
			s, err = db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, "sticky", s.RateLimitRetryPolicy, "restart cannot inherit transport again")
			for _, policy := range []string{"off", "rotate", "sticky"} {
				s.RateLimitRetryPolicy = policy
				require.NoError(t, db.UpdateSystemSettings(t.Context(), s))
				require.NoError(t, db.Close())
				db = reopen()
				s, err = db.GetSystemSettings(t.Context())
				require.NoError(t, err)
				require.Equal(t, policy, s.RateLimitRetryPolicy)
				require.Equal(t, 4, s.MaxRateLimitRetries)
				require.Equal(t, 21, s.BPSAttachmentAccountConcurrency)
			}
		})
	}
}
