package admin

import (
	"encoding/json"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestAccountQuotaRevisionProjectionContainsNoCredentials(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	row := &database.AccountRow{ID: 7, CredentialGeneration: 9, Credentials: map[string]interface{}{"access_token": "secret-access", "refresh_token": "secret-refresh", "email": "synthetic@example.test"}}
	response := (&Handler{store: store}).buildAccountResponse(row, nil, nil, nil, nil, false)
	require.EqualValues(t, 9, response.CredentialGeneration)
	data, err := json.Marshal(response)
	require.NoError(t, err)
	require.Contains(t, string(data), `"credential_generation":9`)
	require.NotContains(t, string(data), "secret-access")
	require.NotContains(t, string(data), "secret-refresh")
}
