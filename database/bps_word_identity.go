package database

import (
	"context"
	"database/sql"
	"errors"
)

// ResolveBPSWordIteration assigns one iteration to a logical inference input,
// not to an HTTP attempt or to each individual parallel tool result.
func (db *DB) ResolveBPSWordIteration(ctx context.Context, turnKey, stepKey string) (iteration int64, reused bool, err error) {
	if !ValidSessionOperationKey(turnKey) || !ValidSessionOperationKey(stepKey) {
		return 0, false, errors.New("invalid Word BPS turn scope")
	}
	err = db.conn.QueryRowContext(ctx, `SELECT iteration FROM bps_word_steps WHERE turn_key=$1 AND step_key=$2`, turnKey, stepKey).Scan(&iteration)
	if err == nil {
		return iteration, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO bps_word_turns(turn_key,iteration) VALUES ($1,0) ON CONFLICT(turn_key) DO NOTHING`, turnKey); err != nil {
			return err
		}
		// Also locks the row on PostgreSQL; SQLite's write transaction serializes
		// writers across instances. Read the step only after taking this lock.
		if _, err := tx.ExecContext(ctx, `UPDATE bps_word_turns SET iteration=iteration WHERE turn_key=$1`, turnKey); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT iteration FROM bps_word_steps WHERE turn_key=$1 AND step_key=$2`, turnKey, stepKey).Scan(&iteration)
		if err == nil {
			reused = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := tx.QueryRowContext(ctx, `UPDATE bps_word_turns SET iteration=iteration+1 WHERE turn_key=$1 RETURNING iteration`, turnKey).Scan(&iteration); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bps_word_steps(turn_key,step_key,iteration) VALUES ($1,$2,$3)`, turnKey, stepKey, iteration)
		return err
	})
	return
}
