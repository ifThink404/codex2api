package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInitialSessionSettingMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "existing", CodexInitialSessionMaxAgeSeconds: 12}))
	s, err := db.GetSystemSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 12, s.CodexInitialSessionMaxAgeSeconds)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	s, err = db.GetSystemSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 12, s.CodexInitialSessionMaxAgeSeconds)
	_, err = db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN codex_initial_session_max_age_seconds")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	s, err = db.GetSystemSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 60, s.CodexInitialSessionMaxAgeSeconds)
	require.Equal(t, "existing", s.SiteName)
	require.False(t, s.CodexInitialSessionAgeCheckDisabled)
	for _, disabled := range []bool{true, false, true} {
		s.CodexInitialSessionAgeCheckDisabled = disabled
		require.NoError(t, db.UpdateSystemSettings(ctx, s))
		require.NoError(t, db.Close())
		db, err = New("sqlite", path)
		require.NoError(t, err)
		s, err = db.GetSystemSettings(ctx)
		require.NoError(t, err)
		require.Equal(t, disabled, s.CodexInitialSessionAgeCheckDisabled)
		require.Equal(t, 60, s.CodexInitialSessionMaxAgeSeconds)
	}
	_, err = db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN codex_initial_session_age_check_disabled")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	s, err = db.GetSystemSettings(ctx)
	require.NoError(t, err)
	require.False(t, s.CodexInitialSessionAgeCheckDisabled, "old installations keep age validation enabled")
	for _, n := range []int{1, 86400, 0, -1, 86401} {
		s.CodexInitialSessionMaxAgeSeconds = n
		s.CodexWebSearchProxyLocation = true
		s.PreservePromptFilterCustomPatterns = true
		s.PreservePromptFilterReviewAPIKey = true
		require.NoError(t, db.UpdateSystemSettings(ctx, s))
		got, err := db.GetSystemSettings(ctx)
		require.NoError(t, err)
		require.Equal(t, NormalizeCodexInitialSessionMaxAgeSeconds(n), got.CodexInitialSessionMaxAgeSeconds)
		require.True(t, got.CodexWebSearchProxyLocation)
	}
}
