package database

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

const DefaultBPSRoundConvergenceLimit = 100
const MaxBPSRoundConvergenceLimit = 1000000

func NormalizeBPSRoundConvergenceLimit(value int) int {
	if value < 1 || value > MaxBPSRoundConvergenceLimit {
		return DefaultBPSRoundConvergenceLimit
	}
	return value
}

type BPSRoundIdentity struct {
	Generation int64
	Iteration  int64
	RoundLimit int
}

// ResolveBPSRoundIdentity assigns a durable position in an account's task batch.
// A retry keeps its assignment even after rotation, settings changes or restart.
func (db *DB) ResolveBPSRoundIdentity(ctx context.Context, accountKey, stepKey string, limit int) (identity BPSRoundIdentity, reused bool, err error) {
	if !ValidSessionOperationKey(accountKey) || !ValidSessionOperationKey(stepKey) || limit < 1 || limit > MaxBPSRoundConvergenceLimit {
		return identity, false, errors.New("invalid BPS round convergence scope or limit")
	}
	err = db.conn.QueryRowContext(ctx, `SELECT generation,iteration,round_limit FROM bps_round_steps WHERE account_key=$1 AND step_key=$2`, accountKey, stepKey).Scan(&identity.Generation, &identity.Iteration, &identity.RoundLimit)
	if err == nil {
		return identity, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return identity, false, err
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO bps_round_tasks(account_key,generation,iteration,round_limit) VALUES($1,0,0,$2) ON CONFLICT(account_key) DO NOTHING`, accountKey, limit); err != nil {
			return err
		}
		// Serialize allocation across processes, not only goroutines.
		if _, err := tx.ExecContext(ctx, `UPDATE bps_round_tasks SET iteration=iteration WHERE account_key=$1`, accountKey); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT generation,iteration,round_limit FROM bps_round_steps WHERE account_key=$1 AND step_key=$2`, accountKey, stepKey).Scan(&identity.Generation, &identity.Iteration, &identity.RoundLimit)
		if err == nil {
			reused = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT generation,iteration,round_limit FROM bps_round_tasks WHERE account_key=$1`, accountKey).Scan(&identity.Generation, &identity.Iteration, &identity.RoundLimit); err != nil {
			return err
		}
		if identity.Iteration >= int64(identity.RoundLimit) {
			if identity.Generation == math.MaxInt64 {
				return errors.New("BPS round generation exhausted")
			}
			identity.Generation++
			identity.Iteration = 0
			// Finish an existing batch under its original limit. A new setting
			// takes effect on the next batch without rewriting prior IDs.
			identity.RoundLimit = limit
		}
		identity.Iteration++
		if _, err := tx.ExecContext(ctx, `UPDATE bps_round_tasks SET generation=$2,iteration=$3,round_limit=$4 WHERE account_key=$1`, accountKey, identity.Generation, identity.Iteration, identity.RoundLimit); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bps_round_steps(account_key,step_key,generation,iteration,round_limit) VALUES($1,$2,$3,$4,$5)`, accountKey, stepKey, identity.Generation, identity.Iteration, identity.RoundLimit)
		return err
	})
	return
}
