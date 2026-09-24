package database

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPromptFilterNewAPIBindingSecretRotationPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	conn, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	conn.SetMaxOpenConns(1)
	// Shadow the production table on this connection without changing any
	// persistent data or requiring the full application schema.
	_, err = conn.ExecContext(t.Context(), strings.Replace(postgresPromptFilterNewAPIBindingsDDL, "CREATE TABLE IF NOT EXISTS", "CREATE TEMP TABLE", 1))
	require.NoError(t, err)
	db := &DB{conn: conn, driver: "postgres"}
	oldSecret := strings.Repeat("a", 32)
	newSecret := strings.Repeat("b", 32)
	finalSecret := strings.Repeat("c", 32)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO prompt_filter_newapi_bindings(api_key_id, platform_code, secret) VALUES (1, 'rotation-test', $1)`, oldSecret)
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
	require.NoError(t, db.ReplacePromptFilterNewAPIBindingSecretAt(t.Context(), 1, newSecret, &expiresAt))
	rotated, err := db.GetPromptFilterNewAPIBinding(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, newSecret, rotated.Secret)
	require.Equal(t, oldSecret, rotated.PreviousSecret)
	require.NotNil(t, rotated.PreviousSecretExpiresAt)
	require.True(t, expiresAt.Equal(*rotated.PreviousSecretExpiresAt))
	require.NoError(t, db.ReplacePromptFilterNewAPIBindingSecretAt(t.Context(), 1, finalSecret, nil))
	rotated, err = db.GetPromptFilterNewAPIBinding(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, finalSecret, rotated.Secret)
	require.Empty(t, rotated.PreviousSecret)
	require.Nil(t, rotated.PreviousSecretExpiresAt)
	require.ErrorIs(t, db.ReplacePromptFilterNewAPIBindingSecretAt(t.Context(), 999, newSecret, nil), sql.ErrNoRows)
}
