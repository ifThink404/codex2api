package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/codex2api/security"
)

// Observations are evidence, never an automatic account allowlist.
type AccountModelObservation struct {
	Model      string `json:"model"`
	Transport  string `json:"transport"`
	Source     string `json:"source"`
	Outcome    string `json:"outcome"`
	ObservedAt int64  `json:"observed_at"`
}

const accountModelObservationsSchema = `CREATE TABLE IF NOT EXISTS account_model_observations (
 account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 credential_generation BIGINT NOT NULL,
 model TEXT NOT NULL,
 transport TEXT NOT NULL,
 source TEXT NOT NULL,
 outcome TEXT NOT NULL,
 observed_at BIGINT NOT NULL,
 PRIMARY KEY (account_id, model, transport, source)
);`

func (db *DB) SaveAccountModelObservations(ctx context.Context, id, generation int64, observations []AccountModelObservation) error {
	if len(observations) == 0 {
		return nil
	}
	if id <= 0 || len(observations) > 512 {
		return fmt.Errorf("invalid model observation batch")
	}
	for _, o := range observations {
		if err := security.ValidateModelName(o.Model); err != nil {
			return err
		}
		if o.Transport != "codex" && o.Transport != "bps" {
			return fmt.Errorf("invalid model observation transport")
		}
		if (o.Source != "manifest" || o.Outcome != "listed") && (o.Source != "probe" || (o.Outcome != "available" && o.Outcome != "unsupported" && o.Outcome != "throttled" && o.Outcome != "error")) {
			return fmt.Errorf("invalid model observation result")
		}
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT credential_generation FROM accounts WHERE id=$1 AND deleted_at IS NULL`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var current int64
		if err := tx.QueryRowContext(ctx, query, id).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if current != generation {
			return nil
		}
		for _, o := range observations {
			_, err := tx.ExecContext(ctx, `INSERT INTO account_model_observations(account_id,credential_generation,model,transport,source,outcome,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT(account_id,model,transport,source) DO UPDATE SET credential_generation=excluded.credential_generation,outcome=excluded.outcome,observed_at=excluded.observed_at
 WHERE account_model_observations.credential_generation<>excluded.credential_generation OR account_model_observations.observed_at<=excluded.observed_at`, id, generation, strings.ToLower(strings.TrimSpace(o.Model)), o.Transport, o.Source, o.Outcome, o.ObservedAt)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) ListAccountModelObservations(ctx context.Context, ids []int64) (map[int64][]AccountModelObservation, error) {
	result := make(map[int64][]AccountModelObservation)
	for len(ids) > 0 {
		n := min(512, len(ids))
		args := make([]any, n)
		slots := make([]string, n)
		for i, id := range ids[:n] {
			args[i] = id
			slots[i] = fmt.Sprintf("$%d", i+1)
		}
		rows, err := db.conn.QueryContext(ctx, `SELECT o.account_id,o.model,o.transport,o.source,o.outcome,o.observed_at FROM account_model_observations o JOIN accounts a ON a.id=o.account_id AND a.credential_generation=o.credential_generation WHERE a.deleted_at IS NULL AND o.account_id IN (`+strings.Join(slots, ",")+`) ORDER BY o.account_id,o.model,o.observed_at DESC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var o AccountModelObservation
			if err := rows.Scan(&id, &o.Model, &o.Transport, &o.Source, &o.Outcome, &o.ObservedAt); err != nil {
				rows.Close()
				return nil, err
			}
			result[id] = append(result[id], o)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	return result, nil
}
