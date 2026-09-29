package database

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

const DefaultBPSRoundConvergenceLimit = 100
const MaxBPSRoundConvergenceLimit = 1000000
const DefaultBPSRoundTaskLifetimeHours = 24
const MaxBPSRoundTaskLifetimeHours = 8760

func NormalizeBPSRoundTaskLifetimeHours(value int) int {
	if value < 1 || value > MaxBPSRoundTaskLifetimeHours {
		return DefaultBPSRoundTaskLifetimeHours
	}
	return value
}

func NormalizeBPSRoundConvergenceLimit(value int) int {
	if value < 1 || value > MaxBPSRoundConvergenceLimit {
		return DefaultBPSRoundConvergenceLimit
	}
	return value
}

type BPSRoundIdentity struct {
	Generation    int64
	Iteration     int64
	RoundLimit    int
	LifetimeHours int
}

type BPSRoundBatchActivity struct {
	StartedAtMS  int64
	LastSentAtMS int64
}

// ResolveBPSRoundIdentity assigns a durable position in an account/model/effort batch.
// accountKey is the caller's opaque account, model and reasoning partition key.
// A retry keeps its assignment even after rotation, settings changes or restart.
func (db *DB) ResolveBPSRoundIdentity(ctx context.Context, accountKey, stepKey string, limit, hours int) (identity BPSRoundIdentity, reused bool, err error) {
	return db.resolveBPSRoundIdentity(ctx, accountKey, stepKey, limit, hours, time.Now)
}

func (db *DB) resolveBPSRoundIdentity(ctx context.Context, accountKey, stepKey string, limit, hours int, now func() time.Time) (identity BPSRoundIdentity, reused bool, err error) {
	if hours < 1 || hours > MaxBPSRoundTaskLifetimeHours {
		return identity, false, errors.New("invalid BPS round task lifetime")
	}
	return db.resolveBPSRoundCounter(ctx, accountKey, stepKey, limit, now, hours)
}

// ResolveBPSTurnQuestionIdentity counts distinct user questions, not inference
// steps. Callers scope it by task and verified user; tool continuations reuse
// the question key. Task expiry is handled by the outer timed-task allocator.
func (db *DB) ResolveBPSTurnQuestionIdentity(ctx context.Context, userKey, questionKey string, limit int) (BPSRoundIdentity, bool, error) {
	return db.resolveBPSRoundCounter(ctx, userKey, questionKey, limit, time.Now, 0)
}

func (db *DB) resolveBPSRoundCounter(ctx context.Context, accountKey, stepKey string, limit int, now func() time.Time, lifetimeHours int) (identity BPSRoundIdentity, reused bool, err error) {
	if !ValidSessionOperationKey(accountKey) || !ValidSessionOperationKey(stepKey) || limit < 1 || limit > MaxBPSRoundConvergenceLimit {
		return identity, false, errors.New("invalid BPS round convergence scope or limit")
	}
	var updatedAt int64
	err = db.conn.QueryRowContext(ctx, `SELECT generation,iteration,round_limit,lifetime_hours,updated_at FROM bps_round_steps WHERE account_key=$1 AND step_key=$2`, accountKey, stepKey).Scan(&identity.Generation, &identity.Iteration, &identity.RoundLimit, &identity.LifetimeHours, &updatedAt)
	if err == nil {
		db.touchBPSIdentity(ctx, updatedAt, `UPDATE bps_round_steps SET updated_at=$3 WHERE account_key=$1 AND step_key=$2`, accountKey, stepKey)
		db.touchBPSIdentity(ctx, updatedAt, `UPDATE bps_round_tasks SET updated_at=$2 WHERE account_key=$1`, accountKey)
		return identity, true, nil
	}
	stamp := now().Unix()
	if !errors.Is(err, sql.ErrNoRows) {
		return identity, false, err
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO bps_round_tasks(account_key,generation,iteration,round_limit,lifetime_hours,updated_at) VALUES($1,0,0,$2,$3,$4) ON CONFLICT(account_key) DO NOTHING`, accountKey, limit, lifetimeHours, stamp); err != nil {
			return err
		}
		// Serialize allocation across processes, not only goroutines.
		if _, err := tx.ExecContext(ctx, `UPDATE bps_round_tasks SET iteration=iteration, updated_at=$2 WHERE account_key=$1`, accountKey, stamp); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT generation,iteration,round_limit,lifetime_hours FROM bps_round_steps WHERE account_key=$1 AND step_key=$2`, accountKey, stepKey).Scan(&identity.Generation, &identity.Iteration, &identity.RoundLimit, &identity.LifetimeHours)
		if err == nil {
			reused = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT generation,iteration,round_limit,lifetime_hours FROM bps_round_tasks WHERE account_key=$1`, accountKey).Scan(&identity.Generation, &identity.Iteration, &identity.RoundLimit, &identity.LifetimeHours); err != nil {
			return err
		}
		// No start record means the batch was only allocated, not sent yet.
		// Only the first inference send starts the fixed timer; activity and
		// restart never extend it. Question counters have no independent timer.
		expired := false
		if lifetimeHours > 0 {
			var startedAtMS int64
			err = tx.QueryRowContext(ctx, `SELECT started_at_unix_ms FROM bps_round_batches WHERE account_key=$1 AND generation=$2`, accountKey, identity.Generation).Scan(&startedAtMS)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			expired = err == nil && now().Sub(time.UnixMilli(startedAtMS)) >= time.Duration(identity.LifetimeHours)*time.Hour
		}
		if identity.Iteration >= int64(identity.RoundLimit) || expired {
			if identity.Generation == math.MaxInt64 {
				return errors.New("BPS round generation exhausted")
			}
			identity.Generation++
			identity.Iteration = 0
			// Finish an existing batch under its original limit. A new setting
			// takes effect on the next batch without rewriting prior IDs.
			identity.RoundLimit = limit
			identity.LifetimeHours = lifetimeHours
		}
		identity.Iteration++
		if _, err := tx.ExecContext(ctx, `UPDATE bps_round_tasks SET generation=$2,iteration=$3,round_limit=$4,lifetime_hours=$5 WHERE account_key=$1`, accountKey, identity.Generation, identity.Iteration, identity.RoundLimit, identity.LifetimeHours); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bps_round_steps(account_key,step_key,generation,iteration,round_limit,lifetime_hours,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, accountKey, stepKey, identity.Generation, identity.Iteration, identity.RoundLimit, identity.LifetimeHours, stamp)
		return err
	})
	return
}

// TouchBPSRoundIdentity records activity immediately before each inference send.
// Later sends never extend the lifetime measured from the first send.
// Retries update only their assigned generation, never a newer batch's timer.
func (db *DB) TouchBPSRoundIdentity(ctx context.Context, accountKey string, generation int64) (BPSRoundBatchActivity, error) {
	return db.touchBPSRoundIdentity(ctx, accountKey, generation, time.Now())
}

func (db *DB) touchBPSRoundIdentity(ctx context.Context, accountKey string, generation int64, now time.Time) (activity BPSRoundBatchActivity, err error) {
	if !ValidSessionOperationKey(accountKey) || generation < 0 {
		return activity, errors.New("invalid BPS round batch")
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		// Use the same cross-process lock as allocation so expiry decisions see
		// the first send. Older concurrent sends cannot move activity back.
		if _, err := tx.ExecContext(ctx, `UPDATE bps_round_tasks SET iteration=iteration, updated_at=$2 WHERE account_key=$1`, accountKey, now.Unix()); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `INSERT INTO bps_round_batches(account_key,generation,started_at_unix_ms,last_sent_at_unix_ms,updated_at) VALUES($1,$2,$3,$3,$4)
			ON CONFLICT(account_key,generation) DO UPDATE SET last_sent_at_unix_ms=CASE WHEN EXCLUDED.last_sent_at_unix_ms > bps_round_batches.last_sent_at_unix_ms THEN EXCLUDED.last_sent_at_unix_ms ELSE bps_round_batches.last_sent_at_unix_ms END, updated_at=EXCLUDED.updated_at
			RETURNING started_at_unix_ms,last_sent_at_unix_ms`, accountKey, generation, now.UnixMilli(), now.Unix()).Scan(&activity.StartedAtMS, &activity.LastSentAtMS)
	})
	return activity, err
}
