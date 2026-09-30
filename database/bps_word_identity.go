package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ResolveBPSWordIteration assigns one iteration to a logical inference input,
// not to an HTTP attempt or to each individual parallel tool result.
func (db *DB) ResolveBPSWordIteration(ctx context.Context, turnKey, stepKey string) (iteration int64, reused bool, err error) {
	if !ValidSessionOperationKey(turnKey) || !ValidSessionOperationKey(stepKey) {
		return 0, false, errors.New("invalid Word BPS turn scope")
	}
	var updatedAt int64
	err = db.conn.QueryRowContext(ctx, `SELECT iteration, updated_at FROM bps_word_steps WHERE turn_key=$1 AND step_key=$2`, turnKey, stepKey).Scan(&iteration, &updatedAt)
	if err == nil {
		db.touchBPSIdentity(ctx, updatedAt, `UPDATE bps_word_steps SET updated_at=$3 WHERE turn_key=$1 AND step_key=$2`, turnKey, stepKey)
		db.touchBPSIdentity(ctx, updatedAt, `UPDATE bps_word_turns SET updated_at=$2 WHERE turn_key=$1`, turnKey)
		return iteration, true, nil
	}
	now := time.Now().Unix()
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO bps_word_turns(turn_key,iteration,updated_at) VALUES ($1,0,$2) ON CONFLICT(turn_key) DO NOTHING`, turnKey, now); err != nil {
			return err
		}
		// Also locks the row on PostgreSQL; SQLite's write transaction serializes
		// writers across instances. Read the step only after taking this lock.
		if _, err := tx.ExecContext(ctx, `UPDATE bps_word_turns SET iteration=iteration, updated_at=$2 WHERE turn_key=$1`, turnKey, now); err != nil {
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
		_, err = tx.ExecContext(ctx, `INSERT INTO bps_word_steps(turn_key,step_key,iteration,updated_at) VALUES ($1,$2,$3,$4)`, turnKey, stepKey, iteration, now)
		return err
	})
	return
}
