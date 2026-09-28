package database

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// This column is managed separately from ordinary settings writes, so saving
// an older settings form cannot overwrite a just-edited builtin rule.
func (db *DB) CompareAndSwapPromptFilterBuiltinOverrides(ctx context.Context, expected, replacement string) (bool, error) {
	if !json.Valid([]byte(expected)) || !json.Valid([]byte(replacement)) {
		return false, errors.New("invalid builtin override JSON")
	}
	var swapped bool
	err := db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET prompt_filter_builtin_overrides=$1 WHERE id=1 AND COALESCE(NULLIF(TRIM(prompt_filter_builtin_overrides), ''), '[]')=$2`, replacement, strings.TrimSpace(expected))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		swapped = count == 1
		return err
	})
	return swapped, err
}
