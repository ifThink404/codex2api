package proxy

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCodexAccountTestRootPersistenceAndIsolation(test *testing.T) {
	dbPath := filepath.Join(test.TempDir(), "test-roots.db")
	db, err := database.New("sqlite", dbPath)
	require.NoError(test, err)
	closed := false
	test.Cleanup(func() {
		if !closed {
			require.NoError(test, db.Close())
		}
	})
	account := &auth.Account{DBID: 42, AccountID: "workspace-a", AccessToken: "old-token"}
	var wg sync.WaitGroup
	values, failures := make(chan string, 8), make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			value, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
			values <- value
			failures <- err
		})
	}
	wg.Wait()
	root := <-values
	for range 8 {
		require.NoError(test, <-failures)
	}
	for range 7 {
		require.Equal(test, root, <-values)
	}
	parsed, err := uuid.Parse(root)
	require.NoError(test, err)
	require.Equal(test, uuid.Version(7), parsed.Version())
	require.NoError(test, db.Close())
	db, err = database.New("sqlite", dbPath)
	require.NoError(test, err)
	reloaded := &auth.Account{DBID: 42, AccountID: "workspace-a", AccessToken: "refreshed-token", CodexInstallationID: "new-device"}
	resolve := func(account *auth.Account) string {
		value, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
		require.NoError(test, err)
		return value
	}
	require.Equal(test, root, resolve(reloaded))
	reloaded.DBID = 43
	require.NotEqual(test, root, resolve(reloaded))
	reloaded.DBID = 42
	reloaded.CustomHeaders = map[string]string{"Chatgpt-Account-Id": "workspace-b"}
	require.NotEqual(test, root, resolve(reloaded))
	_, err = ResolveCodexAccountTestSessionID(test.Context(), db, nil)
	require.Error(test, err)
	require.NoError(test, db.Close())
	closed = true
	value, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
	require.Error(test, err)
	require.Empty(test, value, "database failure must not silently generate a new root")
}

func TestCodexAccountTestOwnerExcludesOrdinaryAnonymousRequests(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "test-owner.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	account := &auth.Account{DBID: 42, AccountID: "workspace-a"}
	session, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
	require.NoError(test, err)
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"session_id":"` + session + `","thread_id":"` + session + `"}}`)
	claim := func(ctx context.Context) error {
		fingerprint := NewCodexTransportFingerprint(account, nil, body, session, ctx)
		return fingerprint.ClaimSessionIdentity(ctx, account, "")
	}
	first := WithCodexAccountTestIdentityStore(test.Context(), db, account)
	second := WithCodexAccountTestIdentityStore(test.Context(), db, account)
	require.NoError(test, claim(first))
	require.NoError(test, claim(second))
	require.Equal(test, WebsocketTransportOwner(first, ""), WebsocketTransportOwner(second, ""))
	require.Error(test, claim(WithCodexIdentityStore(test.Context(), db)))
}

func TestCodexAccountTestRootRotatesAtLocalMidnight(test *testing.T) {
	zone := time.FixedZone("UTC+08", 8*60*60)
	beforeMidnight := time.Date(2026, 9, 23, 23, 59, 59, 0, zone)
	account := &auth.Account{DBID: 42, AccountID: "workspace-a"}
	for _, persistent := range []bool{false, true} {
		name := "without_database"
		if persistent {
			name = "persisted_across_restart"
		}
		test.Run(name, func(test *testing.T) {
			var store CodexIdentityStore
			var db *database.DB
			path := filepath.Join(test.TempDir(), "daily-roots.db")
			if persistent {
				var err error
				db, err = database.New("sqlite", path)
				require.NoError(test, err)
				store = db
				test.Cleanup(func() { require.NoError(test, db.Close()) })
			}
			resolve := func(at time.Time) string {
				session, err := resolveCodexAccountTestSessionIDAt(test.Context(), store, account, at)
				require.NoError(test, err)
				return session
			}
			root := resolve(beforeMidnight)
			require.Equal(test, root, resolve(beforeMidnight.Add(-23*time.Hour)), "crossing UTC midnight within the same local day must not rotate the root")
			nextRoot := resolve(beforeMidnight.Add(time.Second))
			require.NotEqual(test, root, nextRoot, "new probes must switch roots at local midnight")
			require.Equal(test, nextRoot, resolve(beforeMidnight.Add(12*time.Hour)))
			parsed, err := uuid.Parse(nextRoot)
			require.NoError(test, err)
			require.Equal(test, uuid.Version(7), parsed.Version())
			if persistent {
				require.NoError(test, db.Close())
				db, err = database.New("sqlite", path)
				require.NoError(test, err)
				store = db
			}
			require.Equal(test, nextRoot, resolve(beforeMidnight.Add(2*time.Second)), "restart must not create another root within the day")
			require.Equal(test, root, resolve(beforeMidnight), "rotation must not rewrite the preceding day's saved root")
		})
	}
}
