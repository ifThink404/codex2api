package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BPSTaskAffinity is a scheduling preference, never session ownership or a lease.
type BPSTaskAffinity struct {
	AccountID int64
	Revision  int64
}

func (db *DB) ReadBPSTaskAffinity(ctx context.Context, key string) (BPSTaskAffinity, error) {
	var record BPSTaskAffinity
	if !ValidSessionOperationKey(key) {
		return record, errors.New("invalid BPS task affinity key")
	}
	var updatedAt int64
	err := db.conn.QueryRowContext(ctx, `SELECT account_id, revision, updated_at FROM bps_task_affinities WHERE task_key=$1`, key).Scan(&record.AccountID, &record.Revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, nil
	}
	if err == nil {
		db.touchBPSIdentity(ctx, updatedAt, `UPDATE bps_task_affinities SET updated_at=$2 WHERE task_key=$1`, key)
	}
	return record, err
}

// A stale request cannot overwrite a newer preference after a failover. Return
// the current winner so concurrent first requests converge on subsequent calls.
func (db *DB) UpdateBPSTaskAffinity(ctx context.Context, key string, revision, accountID int64) (BPSTaskAffinity, error) {
	var record BPSTaskAffinity
	if !ValidSessionOperationKey(key) || revision < 0 || accountID <= 0 {
		return record, errors.New("invalid BPS task affinity")
	}
	err := db.conn.QueryRowContext(ctx, `INSERT INTO bps_task_affinities(task_key,account_id,revision,updated_at) VALUES($1,$2,1,$4)
		ON CONFLICT(task_key) DO UPDATE SET account_id=excluded.account_id, updated_at=excluded.updated_at,
			revision=CASE WHEN bps_task_affinities.account_id=excluded.account_id THEN bps_task_affinities.revision ELSE bps_task_affinities.revision+1 END
		WHERE bps_task_affinities.revision=$3 RETURNING account_id,revision`, key, accountID, revision, time.Now().Unix()).Scan(&record.AccountID, &record.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ReadBPSTaskAffinity(ctx, key)
	}
	return record, err
}
