package database

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

var ErrCodexIdentityConflict = errors.New("codex session identity belongs to another owner")

// All fields are already one-way hashes. Keep them out of the public error text.
type CodexIdentityConflictError struct {
	IdentityKey   string
	ExistingOwner string
}

func (e *CodexIdentityConflictError) Error() string { return ErrCodexIdentityConflict.Error() }
func (e *CodexIdentityConflictError) Unwrap() error { return ErrCodexIdentityConflict }

func (db *DB) ensureCodexIdentityClaimsTable(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS codex_identity_claims (identity_key TEXT PRIMARY KEY, owner_key TEXT NOT NULL)`)
	return err
}

func (db *DB) ClaimCodexIdentities(ctx context.Context, keys []string, owner string) error {
	if !ValidSessionOperationKey(owner) || len(keys) > 32 {
		return errors.New("invalid codex identity claim")
	}
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	for _, key := range keys {
		if !ValidSessionOperationKey(key) {
			return errors.New("invalid codex identity key")
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, key := range keys {
			if _, err := tx.ExecContext(ctx, `INSERT INTO codex_identity_claims(identity_key,owner_key) VALUES ($1,$2) ON CONFLICT(identity_key) DO NOTHING`, key, owner); err != nil {
				return err
			}
			var existing string
			if err := tx.QueryRowContext(ctx, `SELECT owner_key FROM codex_identity_claims WHERE identity_key=$1`, key).Scan(&existing); err != nil {
				return err
			}
			if existing != owner {
				return &CodexIdentityConflictError{IdentityKey: key, ExistingOwner: existing}
			}
		}
		return nil
	})
}
