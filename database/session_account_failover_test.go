package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sessionAccountFailoverFixture struct {
	db         *DB
	path       string
	input      SessionAccountFailover
	record     SessionContinuityRecord
	admissions UserWindowAdmissionState
}

func newSessionAccountFailoverFixture(test *testing.T) *sessionAccountFailoverFixture {
	test.Helper()
	path := filepath.Join(test.TempDir(), "failover.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	fixture := &sessionAccountFailoverFixture{db: db, path: path}
	test.Cleanup(func() { require.NoError(test, fixture.db.Close()) })
	at := time.Now().UTC().Truncate(time.Second)
	fixture.input = SessionAccountFailover{
		RootKey: strings.Repeat("a", 64), AffinityKey: "affinity", WindowSubject: "user",
		WindowRoot: "window-root", WindowGrantID: "grant", ExpectedAccountID: 1,
		AccountID: 2, Reason: "account_unavailable", At: at,
	}
	completedNumber := uint64(40)
	fixture.record = SessionContinuityRecord{
		AccountID: 1, ThreadID: "thread", Number: 41, NumberKnown: true,
		LastSeen: at.Add(-time.Minute), LastCompleted: at.Add(-2 * time.Minute),
		CompletedNumber: &completedNumber, LastStatus: 200,
	}
	upgradedAt := at.Add(-10 * time.Minute)
	fixture.admissions = UserWindowAdmissionState{
		Windows: map[string]*UserWindowGrant{
			"window-root": {
				ID: "grant", Root: "window-root", CreatedAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour),
				PendingUntil: at.Add(-59 * time.Minute), Confirmed: true, Expanded: true,
				Multiplier: 1.75, ExtraLimit: 3, OwnerAccountID: 1, OwnerKey: "affinity", UpgradedAt: &upgradedAt,
			},
			"other-root": {
				ID: "other-grant", Root: "other-root", CreatedAt: at.Add(-2 * time.Hour), ExpiresAt: at.Add(-time.Hour),
				Confirmed: true, NoWindow: true, Multiplier: 1, OwnerAccountID: 9, OwnerKey: "other-affinity",
			},
		},
		Reservations: map[string]map[string]time.Time{
			"window-root": {"reservation": at.Add(time.Minute)},
			"other-root":  {"expired-reservation": at.Add(-time.Hour)},
		},
	}
	_, err = db.CommitSessionContinuity(test.Context(), fixture.input.RootKey, fixture.record)
	require.NoError(test, err)
	require.NoError(test, db.UpdateUserWindowAdmissions(test.Context(), fixture.input.WindowSubject, func(state *UserWindowAdmissionState) error {
		*state = fixture.admissions
		return nil
	}))
	_, err = db.conn.ExecContext(test.Context(), `INSERT INTO session_blacklist(session_key,user_id,session_id,identity_data,locked,updated_at) VALUES($1,'user','session','{}',1,$2)`, fixture.input.RootKey, at.Add(-time.Hour).UnixMilli())
	require.NoError(test, err)
	return fixture
}

func (fixture *sessionAccountFailoverFixture) snapshot(test *testing.T) []string {
	test.Helper()
	var root, grants, blacklist string
	var updatedAt int64
	err := fixture.db.conn.QueryRowContext(test.Context(), `SELECT state,updated_at FROM codex_session_continuity LIMIT 1`).Scan(&root, &updatedAt)
	if !errors.Is(err, sql.ErrNoRows) {
		require.NoError(test, err)
	}
	err = fixture.db.conn.QueryRowContext(test.Context(), `SELECT state FROM prompt_user_window_grants LIMIT 1`).Scan(&grants)
	if !errors.Is(err, sql.ErrNoRows) {
		require.NoError(test, err)
	}
	require.NoError(test, fixture.db.conn.QueryRowContext(test.Context(), `SELECT session_key || user_id || session_id || identity_data || locked || updated_at FROM session_blacklist LIMIT 1`).Scan(&blacklist))
	return []string{root, grants, time.Unix(updatedAt, 0).UTC().Format(time.RFC3339), blacklist}
}

func TestSwitchSessionContinuityAccountPreservesStateAndPersists(test *testing.T) {
	for _, withGrantID := range []bool{true, false} {
		name := "with_grant_id"
		if !withGrantID {
			name = "without_grant_id"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newSessionAccountFailoverFixture(test)
			if !withGrantID {
				fixture.input.WindowGrantID = ""
			}
			before := fixture.snapshot(test)
			_, err := fixture.db.conn.ExecContext(test.Context(), `CREATE TABLE failover_writes (table_name TEXT);
				CREATE TRIGGER failover_grant_write AFTER UPDATE ON prompt_user_window_grants BEGIN INSERT INTO failover_writes VALUES ('grant'); END;
				CREATE TRIGGER failover_root_write AFTER UPDATE ON codex_session_continuity BEGIN INSERT INTO failover_writes VALUES ('root'); END`)
			require.NoError(test, err)
			record, grant, err := fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
			require.NoError(test, err)
			expected := fixture.record
			expected.AccountID, expected.PreviousAccountID, expected.FailoverCount = 2, 1, 1
			expected.LastFailoverAt, expected.LastFailoverReason = fixture.input.At, fixture.input.Reason
			require.Equal(test, expected, record)
			fixture.admissions.Windows[fixture.input.WindowRoot].OwnerAccountID = 2
			require.Equal(test, fixture.admissions.Windows[fixture.input.WindowRoot], grant)
			var writes string
			require.NoError(test, fixture.db.conn.QueryRowContext(test.Context(), `SELECT group_concat(table_name, ',') FROM (SELECT table_name FROM failover_writes ORDER BY rowid)`).Scan(&writes))
			require.Equal(test, "grant,root,root,grant", writes)
			require.Equal(test, before[2:], fixture.snapshot(test)[2:])
			require.NoError(test, fixture.db.Close())
			fixture.db, err = New("sqlite", fixture.path)
			require.NoError(test, err)
			stored, found, err := fixture.db.ReadSessionContinuity(test.Context(), fixture.input.RootKey)
			require.NoError(test, err)
			require.True(test, found)
			require.Equal(test, expected, stored)
			admissions, err := fixture.db.ReadUserWindowAdmissions(test.Context(), fixture.input.WindowSubject)
			require.NoError(test, err)
			require.Equal(test, fixture.admissions, admissions)
		})
	}
}

func TestSwitchSessionContinuityAccountWithoutGrant(test *testing.T) {
	fixture := newSessionAccountFailoverFixture(test)
	fixture.input.WindowSubject, fixture.input.WindowRoot, fixture.input.WindowGrantID, fixture.input.AffinityKey = "", "", "", ""
	fixture.input.At = time.Time{}
	before := fixture.snapshot(test)
	started := time.Now().UTC()
	record, grant, err := fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
	require.NoError(test, err)
	require.Nil(test, grant)
	require.Equal(test, int64(2), record.AccountID)
	require.Equal(test, uint64(1), record.FailoverCount)
	require.False(test, record.LastFailoverAt.Before(started))
	require.False(test, record.LastFailoverAt.After(time.Now()))
	require.Equal(test, before[1:], fixture.snapshot(test)[1:])
}

func TestSwitchSessionContinuityAccountPendingGrant(test *testing.T) {
	for _, scenario := range []string{"valid", "not_allowed", "expired", "missing_id", "wrong_id", "stale_owner"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newSessionAccountFailoverFixture(test)
			grant := fixture.admissions.Windows[fixture.input.WindowRoot]
			grant.Confirmed, grant.Expanded, grant.Multiplier = false, false, 1
			grant.PendingUntil = fixture.input.At.Add(30 * time.Second)
			fixture.input.AllowPendingWindowGrant = true
			switch scenario {
			case "not_allowed":
				fixture.input.AllowPendingWindowGrant = false
			case "expired":
				grant.PendingUntil = fixture.input.At
			case "missing_id":
				fixture.input.WindowGrantID = ""
			case "wrong_id":
				fixture.input.WindowGrantID = "another"
			case "stale_owner":
				fixture.input.ExpectedAccountID = 9
			}
			require.NoError(test, fixture.db.UpdateUserWindowAdmissions(test.Context(), fixture.input.WindowSubject, func(state *UserWindowAdmissionState) error { *state = fixture.admissions; return nil }))
			before := fixture.snapshot(test)
			record, updated, err := fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
			if scenario != "valid" {
				require.Error(test, err)
				require.Equal(test, before, fixture.snapshot(test), "failed switch must roll back both ownership and grant")
				return
			}
			require.NoError(test, err)
			require.Equal(test, int64(2), record.AccountID)
			require.Equal(test, int64(2), updated.OwnerAccountID)
			require.False(test, updated.Confirmed, "dispatch must still perform normal user-window admission")
			require.Equal(test, grant.ID, updated.ID)
			require.Equal(test, grant.PendingUntil, updated.PendingUntil)
			require.Equal(test, grant.ExpiresAt, updated.ExpiresAt)
			require.Equal(test, 1.0, updated.Multiplier)
			state, err := fixture.db.ReadUserWindowAdmissions(test.Context(), fixture.input.WindowSubject)
			require.NoError(test, err)
			require.Equal(test, fixture.admissions.Reservations, state.Reservations)
		})
	}
}

func TestSwitchSessionContinuityAccountRejectsInvalidState(test *testing.T) {
	for _, testCase := range []struct {
		name     string
		change   func(*sessionAccountFailoverFixture)
		conflict bool
	}{
		{name: "deferred_without_reset", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.DeferOutboundWindow = true }},
		{name: "deferred_with_fabricated_window", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.input.ResetOutboundWindow, fixture.input.DeferOutboundWindow = true, true
			fixture.input.WindowThreadID = "thread"
		}},
		{name: "empty_root", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.RootKey = "" }},
		{name: "missing_root", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.RootKey = "missing" }},
		{name: "zero_expected_account", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.ExpectedAccountID = 0 }},
		{name: "negative_expected_account", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.ExpectedAccountID = -1 }},
		{name: "zero_account", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.AccountID = 0 }},
		{name: "negative_account", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.AccountID = -1 }},
		{name: "same_account", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.AccountID = 1 }},
		{name: "wrong_owner", change: func(fixture *sessionAccountFailoverFixture) { fixture.record.AccountID = 3 }, conflict: true},
		{name: "missing_owner", change: func(fixture *sessionAccountFailoverFixture) { fixture.record.AccountID = 0 }, conflict: true},
		{name: "stale_generation", change: func(fixture *sessionAccountFailoverFixture) { fixture.record.FailoverCount = 1 }, conflict: true},
		{name: "future_generation", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.ExpectedGeneration = 1 }, conflict: true},
		{name: "generation_overflow", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.record.FailoverCount, fixture.input.ExpectedGeneration = ^uint64(0), ^uint64(0)
		}},
		{name: "missing_subject", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.WindowSubject = "missing" }},
		{name: "empty_subject", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.WindowSubject = "" }},
		{name: "empty_window_root", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.WindowRoot = "" }},
		{name: "empty_affinity", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.AffinityKey = "" }},
		{name: "missing_grant", change: func(fixture *sessionAccountFailoverFixture) { delete(fixture.admissions.Windows, "window-root") }},
		{name: "nil_grant", change: func(fixture *sessionAccountFailoverFixture) { fixture.admissions.Windows["window-root"] = nil }},
		{name: "empty_grant_id", change: func(fixture *sessionAccountFailoverFixture) { fixture.admissions.Windows["window-root"].ID = "" }},
		{name: "wrong_grant_id", change: func(fixture *sessionAccountFailoverFixture) { fixture.input.WindowGrantID = "other" }},
		{name: "wrong_grant_root", change: func(fixture *sessionAccountFailoverFixture) { fixture.admissions.Windows["window-root"].Root = "other" }},
		{name: "wrong_grant_affinity", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].OwnerKey = "other"
		}},
		{name: "missing_grant_affinity", change: func(fixture *sessionAccountFailoverFixture) { fixture.admissions.Windows["window-root"].OwnerKey = "" }},
		{name: "wrong_grant_owner", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].OwnerAccountID = 3
		}, conflict: true},
		{name: "missing_grant_owner", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].OwnerAccountID = 0
		}, conflict: true},
		{name: "unconfirmed_grant", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].Confirmed = false
		}},
		{name: "expired_grant", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].ExpiresAt = fixture.input.At.Add(-time.Nanosecond)
		}},
		{name: "expiry_boundary", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].ExpiresAt = fixture.input.At
		}},
		{name: "missing_expiry", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].ExpiresAt = time.Time{}
		}},
		{name: "zero_at_expired_grant", change: func(fixture *sessionAccountFailoverFixture) {
			fixture.admissions.Windows["window-root"].ExpiresAt = fixture.input.At.Add(-time.Minute)
			fixture.input.At = time.Time{}
		}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := newSessionAccountFailoverFixture(test)
			testCase.change(fixture)
			payload, err := json.Marshal(fixture.record)
			require.NoError(test, err)
			_, err = fixture.db.conn.ExecContext(test.Context(), `UPDATE codex_session_continuity SET state=$1`, string(payload))
			require.NoError(test, err)
			payload, err = json.Marshal(fixture.admissions)
			require.NoError(test, err)
			_, err = fixture.db.conn.ExecContext(test.Context(), `UPDATE prompt_user_window_grants SET state=$1`, string(payload))
			require.NoError(test, err)
			before := fixture.snapshot(test)
			record, grant, err := fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
			require.Error(test, err)
			if testCase.conflict {
				require.ErrorIs(test, err, ErrSessionOwnerConflict)
			}
			require.Equal(test, SessionContinuityRecord{}, record)
			require.Nil(test, grant)
			require.Equal(test, before, fixture.snapshot(test))
			var roots, subjects int
			require.NoError(test, fixture.db.conn.QueryRowContext(test.Context(), `SELECT count(*) FROM codex_session_continuity`).Scan(&roots))
			require.NoError(test, fixture.db.conn.QueryRowContext(test.Context(), `SELECT count(*) FROM prompt_user_window_grants`).Scan(&subjects))
			require.Equal(test, 1, roots)
			require.Equal(test, 1, subjects)
		})
	}
}

func TestSwitchSessionContinuityAccountConcurrentSingleWinner(test *testing.T) {
	for _, withGrant := range []bool{true, false} {
		name := "with_grant"
		if !withGrant {
			name = "without_grant"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newSessionAccountFailoverFixture(test)
			second, err := New("sqlite", fixture.path)
			require.NoError(test, err)
			test.Cleanup(func() { require.NoError(test, second.Close()) })
			if !withGrant {
				fixture.input.WindowSubject, fixture.input.WindowRoot, fixture.input.WindowGrantID = "", "", ""
			}
			type result struct {
				record SessionContinuityRecord
				grant  *UserWindowGrant
				err    error
			}
			start := make(chan struct{})
			results := make(chan result, 12)
			for index := range cap(results) {
				db := []*DB{fixture.db, second}[index%2]
				input := fixture.input
				input.AccountID = int64(index + 2)
				go func() {
					<-start
					record, grant, err := db.SwitchSessionContinuityAccount(test.Context(), input)
					results <- result{record, grant, err}
				}()
			}
			close(start)
			var winner SessionContinuityRecord
			var successes, conflicts int
			for range cap(results) {
				result := <-results
				if result.err != nil {
					require.ErrorIs(test, result.err, ErrSessionOwnerConflict)
					require.Equal(test, SessionContinuityRecord{}, result.record)
					require.Nil(test, result.grant)
					conflicts++
					continue
				}
				successes++
				winner = result.record
				if withGrant {
					require.NotNil(test, result.grant)
					require.Equal(test, winner.AccountID, result.grant.OwnerAccountID)
				} else {
					require.Nil(test, result.grant)
				}
			}
			require.Equal(test, 1, successes)
			require.Equal(test, cap(results)-1, conflicts)
			require.Equal(test, uint64(1), winner.FailoverCount)
			stored, found, err := second.ReadSessionContinuity(test.Context(), fixture.input.RootKey)
			require.NoError(test, err)
			require.True(test, found)
			require.Equal(test, winner, stored)
			admissions, err := second.ReadUserWindowAdmissions(test.Context(), "user")
			require.NoError(test, err)
			if withGrant {
				fixture.admissions.Windows["window-root"].OwnerAccountID = winner.AccountID
			}
			require.Equal(test, fixture.admissions, admissions)
		})
	}
}

func TestSwitchSessionContinuityAccountGenerationPreventsABA(test *testing.T) {
	fixture := newSessionAccountFailoverFixture(test)
	_, _, err := fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
	require.NoError(test, err)
	returnInput := fixture.input
	returnInput.ExpectedAccountID, returnInput.AccountID, returnInput.ExpectedGeneration = 2, 1, 1
	returnInput.At = returnInput.At.Add(time.Minute)
	returnInput.Reason = "replacement_unavailable"
	record, grant, err := fixture.db.SwitchSessionContinuityAccount(test.Context(), returnInput)
	require.NoError(test, err)
	require.Equal(test, int64(1), record.AccountID)
	require.Equal(test, int64(2), record.PreviousAccountID)
	require.Equal(test, uint64(2), record.FailoverCount)
	require.Equal(test, returnInput.At, record.LastFailoverAt)
	require.Equal(test, returnInput.Reason, record.LastFailoverReason)
	require.Equal(test, int64(1), grant.OwnerAccountID)
	before := fixture.snapshot(test)
	_, _, err = fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
	require.ErrorIs(test, err, ErrSessionOwnerConflict)
	require.Equal(test, before, fixture.snapshot(test))
	_, err = fixture.db.CommitSessionContinuity(test.Context(), fixture.input.RootKey, fixture.record)
	require.ErrorIs(test, err, ErrSessionOwnerConflict)
	require.Equal(test, before, fixture.snapshot(test))
	committed, err := fixture.db.CommitSessionContinuity(test.Context(), fixture.input.RootKey, record)
	require.NoError(test, err)
	require.Equal(test, record, committed)
	fixture.input.ExpectedGeneration = record.FailoverCount
	record, _, err = fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
	require.NoError(test, err)
	require.Equal(test, uint64(3), record.FailoverCount)
}

func TestSwitchSessionContinuityAccountRollsBackOnStorageError(test *testing.T) {
	for _, testCase := range []struct {
		name    string
		prepare string
		message string
	}{
		{name: "invalid_root_json", prepare: `UPDATE codex_session_continuity SET state='invalid'`},
		{name: "invalid_grant_json", prepare: `UPDATE prompt_user_window_grants SET state='invalid'`},
		{name: "root_write", prepare: `CREATE TRIGGER fail_root AFTER UPDATE ON codex_session_continuity
			WHEN NEW.state <> OLD.state BEGIN SELECT RAISE(ABORT, 'injected root write failure'); END`, message: "injected root write failure"},
		{name: "both_owners_written", prepare: `CREATE TRIGGER fail_grant AFTER UPDATE ON prompt_user_window_grants
			WHEN NEW.state <> OLD.state BEGIN
			SELECT CASE WHEN (SELECT json_extract(state, '$.account_id') FROM codex_session_continuity)=2
			AND json_extract(NEW.state, '$.windows.window-root.owner_account_id')=2
			THEN RAISE(ABORT, 'injected failure after both owners changed')
			ELSE RAISE(ABORT, 'unexpected write order') END; END`, message: "injected failure after both owners changed"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := newSessionAccountFailoverFixture(test)
			_, err := fixture.db.conn.ExecContext(test.Context(), testCase.prepare)
			require.NoError(test, err)
			before := fixture.snapshot(test)
			record, grant, err := fixture.db.SwitchSessionContinuityAccount(test.Context(), fixture.input)
			require.Error(test, err)
			if testCase.message != "" {
				require.ErrorContains(test, err, testCase.message)
			}
			require.Equal(test, SessionContinuityRecord{}, record)
			require.Nil(test, grant)
			require.Equal(test, before, fixture.snapshot(test))
		})
	}
}
