package database

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

const DefaultBPSTurnTaskLifetimeHours = 24
const MaxBPSTurnTaskLifetimeHours = 8760

func NormalizeBPSTurnTaskLifetimeHours(value int) int {
	if value < 1 || value > MaxBPSTurnTaskLifetimeHours {
		return DefaultBPSTurnTaskLifetimeHours
	}
	return value
}

type BPSTurnTaskIdentity struct {
	Generation    int64
	LifetimeHours int
}

// ResolveBPSTurnTaskIdentity assigns new inference steps to a timed task.
// The fixed lifetime starts at the first actual upstream send. Replays retain
// their assigned generation, even after expiry or a process restart.
func (db *DB) ResolveBPSTurnTaskIdentity(ctx context.Context, accountKey, stepKey string, hours int) (BPSTurnTaskIdentity, bool, error) {
	return db.resolveBPSTurnTaskIdentity(ctx, accountKey, stepKey, hours, time.Now)
}

func (db *DB) resolveBPSTurnTaskIdentity(ctx context.Context, accountKey, stepKey string, hours int, now func() time.Time) (identity BPSTurnTaskIdentity, reused bool, err error) {
	if !ValidSessionOperationKey(accountKey) || !ValidSessionOperationKey(stepKey) || hours < 1 || hours > MaxBPSTurnTaskLifetimeHours {
		return identity, false, errors.New("invalid BPS turn task scope or lifetime")
	}
	err = db.conn.QueryRowContext(ctx, `SELECT generation,lifetime_hours FROM bps_turn_steps WHERE account_key=$1 AND step_key=$2`, accountKey, stepKey).Scan(&identity.Generation, &identity.LifetimeHours)
	if err == nil {
		return identity, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return identity, false, err
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO bps_turn_tasks(account_key,generation,lifetime_hours) VALUES($1,0,$2) ON CONFLICT(account_key) DO NOTHING`, accountKey, hours); err != nil {
			return err
		}
		// The same row lock is used for allocation and the first-send timer.
		if _, err := tx.ExecContext(ctx, `UPDATE bps_turn_tasks SET generation=generation WHERE account_key=$1`, accountKey); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT generation,lifetime_hours FROM bps_turn_steps WHERE account_key=$1 AND step_key=$2`, accountKey, stepKey).Scan(&identity.Generation, &identity.LifetimeHours)
		if err == nil {
			reused = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT generation,lifetime_hours FROM bps_turn_tasks WHERE account_key=$1`, accountKey).Scan(&identity.Generation, &identity.LifetimeHours); err != nil {
			return err
		}
		var startedAtMS int64
		err = tx.QueryRowContext(ctx, `SELECT started_at_unix_ms FROM bps_turn_batches WHERE account_key=$1 AND generation=$2`, accountKey, identity.Generation).Scan(&startedAtMS)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && now().Sub(time.UnixMilli(startedAtMS)) >= time.Duration(identity.LifetimeHours)*time.Hour {
			if identity.Generation == math.MaxInt64 {
				return errors.New("BPS turn task generation exhausted")
			}
			identity.Generation++
			// Settings changes apply to the next task; never rewrite an active
			// generation's deadline or any retry assignment.
			identity.LifetimeHours = hours
			if _, err := tx.ExecContext(ctx, `UPDATE bps_turn_tasks SET generation=$2,lifetime_hours=$3 WHERE account_key=$1`, accountKey, identity.Generation, hours); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bps_turn_steps(account_key,step_key,generation,lifetime_hours) VALUES($1,$2,$3,$4)`, accountKey, stepKey, identity.Generation, identity.LifetimeHours)
		return err
	})
	return
}

func (db *DB) TouchBPSTurnTaskIdentity(ctx context.Context, accountKey string, generation int64) (BPSRoundBatchActivity, error) {
	return db.touchBPSTurnTaskIdentity(ctx, accountKey, generation, time.Now())
}

func (db *DB) touchBPSTurnTaskIdentity(ctx context.Context, accountKey string, generation int64, now time.Time) (activity BPSRoundBatchActivity, err error) {
	if !ValidSessionOperationKey(accountKey) || generation < 0 {
		return activity, errors.New("invalid BPS turn task generation")
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE bps_turn_tasks SET generation=generation WHERE account_key=$1`, accountKey); err != nil {
			return err
		}
		// Activity is observable, but never extends the first-send deadline.
		return tx.QueryRowContext(ctx, `INSERT INTO bps_turn_batches(account_key,generation,started_at_unix_ms,last_sent_at_unix_ms) VALUES($1,$2,$3,$3)
			ON CONFLICT(account_key,generation) DO UPDATE SET last_sent_at_unix_ms=CASE WHEN EXCLUDED.last_sent_at_unix_ms > bps_turn_batches.last_sent_at_unix_ms THEN EXCLUDED.last_sent_at_unix_ms ELSE bps_turn_batches.last_sent_at_unix_ms END
			RETURNING started_at_unix_ms,last_sent_at_unix_ms`, accountKey, generation, now.UnixMilli()).Scan(&activity.StartedAtMS, &activity.LastSentAtMS)
	})
	return
}
