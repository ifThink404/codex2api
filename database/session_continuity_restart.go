package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// RestartSessionContinuity keeps the selected owner and incoming numbering while
// atomically advancing the outbound generation. The generation also invalidates
// in-flight requests from the previous segment across gateway instances.
func (db *DB) RestartSessionContinuity(ctx context.Context, key string, expected, next SessionContinuityRecord) (SessionContinuityRecord, error) {
	var record SessionContinuityRecord
	forkRestart := next.LastFailoverReason == "continuity_fork_parent_missing" || next.LastFailoverReason == "continuity_fork_parent_capacity_full"
	if key == "" || next.AccountID <= 0 || next.ThreadID == "" || !next.NumberKnown ||
		(expected.AccountID > 0 && expected.AccountID != next.AccountID) ||
		(forkRestart && expected.AccountID != 0) ||
		(!forkRestart && next.LastFailoverReason != "continuity_window_gap" && next.LastFailoverReason != "continuity_unbound_nonzero") || expected.FailoverCount == ^uint64(0) {
		return record, errors.New("invalid continuity restart")
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		seed, err := json.Marshal(expected)
		if err != nil {
			return err
		}
		// Insert and lock the root, including the first unbound request.
		if _, err = tx.ExecContext(ctx, `INSERT INTO codex_session_continuity(root_key,state,updated_at) VALUES ($1,$2,$3) ON CONFLICT(root_key) DO UPDATE SET state=codex_session_continuity.state`, key, string(seed), next.LastSeen.Unix()); err != nil {
			return err
		}
		var raw string
		if err = tx.QueryRowContext(ctx, `SELECT state FROM codex_session_continuity WHERE root_key=$1`, key).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &record); err != nil {
			return err
		}
		// Concurrent copies of the same restart share the committed segment.
		if record.AccountID == next.AccountID && record.FailoverCount == expected.FailoverCount+1 && record.LastFailoverReason == next.LastFailoverReason && record.ThreadID == next.ThreadID && record.Number == next.Number && record.OutboundWindowReset && record.LossyContextRestart {
			return nil
		}
		if record.AccountID != expected.AccountID || record.FailoverCount != expected.FailoverCount || record.ThreadID != expected.ThreadID || record.NumberKnown != expected.NumberKnown || record.Number != expected.Number {
			return ErrSessionOwnerConflict
		}
		if record.ThreadID != "" && !strings.EqualFold(record.ThreadID, next.ThreadID) {
			return ErrSessionOwnerConflict
		}
		if record.AccountID == 0 {
			record.UpstreamMode = next.UpstreamMode
		}
		record.AccountID, record.PreviousAccountID = next.AccountID, expected.AccountID
		record.ThreadID, record.Number, record.NumberKnown = next.ThreadID, next.Number, true
		record.LastSeen, record.LastFailoverAt, record.LastFailoverReason = next.LastSeen, next.LastSeen, next.LastFailoverReason
		record.FailoverCount++
		record.OutboundWindowReset, record.LossyContextRestart = true, true
		if forkRestart {
			record.PreserveRestartInput = next.PreserveRestartInput
			record.DetachedForkReferences = append([]string(nil), next.DetachedForkReferences...)
		}
		record.OutboundWindowMode = "context-v1"
		record.OutboundWindowBases = map[string]uint64{next.ThreadID: next.Number}
		window := SessionOutboundWindowInput{Number: next.Number}
		record.OutboundWindows = map[string]*SessionOutboundWindowState{next.ThreadID: {Next: 1, Entries: map[string]SessionOutboundWindowEntry{window.key(): {Original: next.Number, Number: 0}}}}
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE codex_session_continuity SET state=$2, updated_at=$3 WHERE root_key=$1`, key, string(payload), record.LastSeen.Unix())
		return err
	})
	if err != nil {
		return SessionContinuityRecord{}, err
	}
	return record, nil
}
