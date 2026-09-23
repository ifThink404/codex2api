package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type SessionAccountFailover struct {
	RootKey                 string
	AffinityKey             string
	WindowSubject           string
	WindowRoot              string
	WindowGrantID           string
	AllowPendingWindowGrant bool
	ExpectedAccountID       int64
	AccountID               int64
	ExpectedGeneration      uint64
	UpstreamMode            string
	Reason                  string
	At                      time.Time
	ResetOutboundWindow     bool
	WindowThreadID          string
	WindowNumber            uint64
	WindowContextID         string
	LossyContextRestart     bool
	PreserveRestartInput    bool
}

func (db *DB) SwitchSessionContinuityAccount(ctx context.Context, input SessionAccountFailover) (SessionContinuityRecord, *UserWindowGrant, error) {
	var record SessionContinuityRecord
	var grant *UserWindowGrant
	if strings.TrimSpace(input.RootKey) == "" || input.ExpectedAccountID <= 0 || input.AccountID <= 0 || input.ExpectedAccountID == input.AccountID && input.UpstreamMode != "bps" {
		return record, nil, errors.New("invalid session account failover")
	}
	if input.WindowSubject != "" {
		if strings.TrimSpace(input.WindowSubject) == "" || strings.TrimSpace(input.WindowRoot) == "" || strings.TrimSpace(input.AffinityKey) == "" {
			return record, nil, errors.New("invalid session account failover window scope")
		}
	} else if input.WindowRoot != "" || input.WindowGrantID != "" {
		return record, nil, errors.New("session account failover window subject is required")
	}
	if input.At.IsZero() {
		input.At = time.Now().UTC()
	}
	if input.ResetOutboundWindow && strings.TrimSpace(input.WindowThreadID) == "" {
		return record, nil, errors.New("session account failover window identity is required")
	}
	err := db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		var state UserWindowAdmissionState
		if input.WindowSubject != "" {
			if _, err := transaction.ExecContext(ctx, `UPDATE prompt_user_window_grants SET state=state WHERE subject=$1`, input.WindowSubject); err != nil {
				return err
			}
			var raw string
			if err := transaction.QueryRowContext(ctx, `SELECT state FROM prompt_user_window_grants WHERE subject=$1`, input.WindowSubject).Scan(&raw); err != nil {
				return fmt.Errorf("session account failover window grant: %w", err)
			}
			if err := json.Unmarshal([]byte(raw), &state); err != nil {
				return err
			}
			grant = state.Windows[input.WindowRoot]
			if grant == nil {
				return errors.New("session account failover window grant is missing")
			}
			if grant.ID == "" || input.WindowGrantID != "" && grant.ID != input.WindowGrantID || grant.Root != input.WindowRoot || grant.OwnerKey != input.AffinityKey {
				return errors.New("invalid session account failover window grant")
			}
			if grant.OwnerAccountID != input.ExpectedAccountID {
				return ErrSessionOwnerConflict
			}
			pendingAllowed := input.AllowPendingWindowGrant && input.WindowGrantID != "" && grant.PendingUntil.After(input.At)
			if !grant.ExpiresAt.After(input.At) || !grant.Confirmed && !pendingAllowed {
				return errors.New("session account failover window grant is unconfirmed or expired")
			}
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE codex_session_continuity SET state=state WHERE root_key=$1`, input.RootKey); err != nil {
			return err
		}
		var raw string
		if err := transaction.QueryRowContext(ctx, `SELECT state FROM codex_session_continuity WHERE root_key=$1`, input.RootKey).Scan(&raw); err != nil {
			return fmt.Errorf("session account failover root: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return err
		}
		if record.AccountID != input.ExpectedAccountID || record.FailoverCount != input.ExpectedGeneration {
			return ErrSessionOwnerConflict
		}
		priorMode := record.UpstreamMode
		if priorMode == "" {
			priorMode = "native"
		}
		nextMode := input.UpstreamMode
		if nextMode == "" {
			nextMode = priorMode
		}
		if (nextMode != "native" && nextMode != "bps") || priorMode == "bps" && nextMode != "bps" || input.AccountID == input.ExpectedAccountID && (priorMode != "native" || nextMode != "bps") {
			return errors.New("invalid upstream route transition")
		}
		if input.UpstreamMode != "" {
			record.UpstreamMode = nextMode
		}
		if record.FailoverCount == ^uint64(0) {
			return errors.New("session account failover generation exhausted")
		}
		record.PreviousAccountID = record.AccountID
		record.AccountID = input.AccountID
		record.FailoverCount++
		record.LastFailoverAt = input.At
		record.LastFailoverReason = input.Reason
		record.OutboundWindowReset = input.ResetOutboundWindow
		record.OutboundWindowBases = nil
		record.OutboundWindows = nil
		record.OutboundWindowMode = ""
		record.LossyContextRestart = input.LossyContextRestart
		record.PreserveRestartInput = input.PreserveRestartInput
		if input.ResetOutboundWindow {
			record.OutboundWindowBases = map[string]uint64{input.WindowThreadID: input.WindowNumber}
			record.OutboundWindowMode = "context-v1"
			window := SessionOutboundWindowInput{Number: input.WindowNumber, ContextID: input.WindowContextID}
			record.OutboundWindows = map[string]*SessionOutboundWindowState{input.WindowThreadID: {Next: 1, Entries: map[string]SessionOutboundWindowEntry{window.key(): {Original: input.WindowNumber, Number: 0}}}}
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE codex_session_continuity SET state=$2 WHERE root_key=$1`, input.RootKey, string(payload)); err != nil {
			return err
		}
		if grant != nil {
			grant.OwnerAccountID = input.AccountID
			payload, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if _, err := transaction.ExecContext(ctx, `UPDATE prompt_user_window_grants SET state=$2 WHERE subject=$1`, input.WindowSubject, string(payload)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return SessionContinuityRecord{}, nil, err
	}
	return record, grant, nil
}
