package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BPS usage-policy block history: one row per block event of an account,
// from the first strike of the cooldown until a background probe clears it.
// Route says which transport of the account was broken: bps (usage-policy
// blocks) or native (the native route breaker of dual-route accounts; Detail
// records its trigger and the requested / upstream-reported models).
// It makes block frequency and recovery time measurable. Cleared rows are
// pruned after BPSPolicyBlockRetention.

const BPSPolicyBlockRetention = 30 * 24 * time.Hour

func (db *DB) migrateBPSPolicyBlocks(ctx context.Context) error {
	id := "BIGSERIAL PRIMARY KEY"
	if db.isSQLite() {
		id = "INTEGER PRIMARY KEY AUTOINCREMENT"
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS bps_policy_blocks (
			id ` + id + `,
			account_id BIGINT NOT NULL,
			blocked_at BIGINT NOT NULL,
			tier INTEGER NOT NULL DEFAULT 1,
			cleared_at BIGINT,
			duration_seconds BIGINT NOT NULL DEFAULT 0,
			probe_count INTEGER NOT NULL DEFAULT 0,
			last_probe_result TEXT NOT NULL DEFAULT '',
			last_probe_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bps_policy_blocks_account ON bps_policy_blocks(account_id, cleared_at)`,
		`CREATE INDEX IF NOT EXISTS idx_bps_policy_blocks_blocked ON bps_policy_blocks(blocked_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, column := range []struct{ name, def string }{
		{"route", "TEXT NOT NULL DEFAULT 'bps'"},
		{"detail", "TEXT NOT NULL DEFAULT ''"},
		{"kind", "TEXT NOT NULL DEFAULT 'policy'"},
	} {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, "bps_policy_blocks", column.name, column.def); err != nil {
				return err
			}
			continue
		}
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE bps_policy_blocks ADD COLUMN IF NOT EXISTS `+column.name+` `+column.def); err != nil {
			return err
		}
	}
	return nil
}

// Broken routes of the block history, and what broke them: the BPS usage
// policy (bps route only) or the degradation breaker (either route).
const (
	BPSRouteBPS    = "bps"
	BPSRouteNative = "native"

	RouteBlockPolicy  = "policy"
	RouteBlockDegrade = "degrade"
)

// BPSPolicyBlock is one block event. ClearedAt is nil while it is active.
type BPSPolicyBlock struct {
	ID              int64      `json:"id"`
	AccountID       int64      `json:"account_id"`
	Route           string     `json:"route"`
	Kind            string     `json:"kind"`
	Detail          string     `json:"detail,omitempty"`
	BlockedAt       time.Time  `json:"blocked_at"`
	Tier            int        `json:"tier"`
	ClearedAt       *time.Time `json:"cleared_at,omitempty"`
	DurationSeconds int64      `json:"duration_seconds"`
	ProbeCount      int        `json:"probe_count"`
	LastProbeResult string     `json:"last_probe_result"`
	LastProbeAt     *time.Time `json:"last_probe_at,omitempty"`
}

// Elapsed is how long the block lasted: until ClearedAt, or until now while
// it is active.
func (b BPSPolicyBlock) Elapsed(now time.Time) time.Duration {
	end := now
	if b.ClearedAt != nil {
		end = *b.ClearedAt
	}
	if end.Before(b.BlockedAt) {
		return 0
	}
	return end.Sub(b.BlockedAt)
}

// OpenBPSPolicyBlock records that account entered a usage-policy cooldown at
// tier: a new row when none is active (blockedAt is the first strike), else
// the active row's tier is raised.
func (db *DB) OpenBPSPolicyBlock(ctx context.Context, accountID int64, blockedAt time.Time, tier int) error {
	return db.OpenRouteBlock(ctx, accountID, BPSRouteBPS, RouteBlockPolicy, blockedAt, tier, "")
}

// OpenRouteBlock is OpenBPSPolicyBlock for one route of the account; detail
// describes a new event (kept on an already active one).
func (db *DB) OpenRouteBlock(ctx context.Context, accountID int64, route, kind string, blockedAt time.Time, tier int, detail string) error {
	if db == nil || db.conn == nil || accountID <= 0 {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE bps_policy_blocks SET tier=$2 WHERE account_id=$1 AND route=$3 AND kind=$4 AND cleared_at IS NULL AND tier < $2`, accountID, tier, route, kind); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bps_policy_blocks WHERE account_id=$1 AND route=$2 AND kind=$3 AND cleared_at IS NULL`, accountID, route, kind).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO bps_policy_blocks(account_id,blocked_at,tier,route,kind,detail) VALUES($1,$2,$3,$4,$5,$6)`, accountID, blockedAt.Unix(), tier, route, kind, clampUsageLogText(detail, 300))
		return err
	})
}

// RecordBPSPolicyProbe notes a probe of the account's active block (and the
// tier it left the account at).
func (db *DB) RecordBPSPolicyProbe(ctx context.Context, accountID int64, at time.Time, result string, tier int) error {
	return db.RecordRouteProbe(ctx, accountID, BPSRouteBPS, RouteBlockPolicy, at, result, tier)
}

// RecordRouteProbe is RecordBPSPolicyProbe for one route of the account.
func (db *DB) RecordRouteProbe(ctx context.Context, accountID int64, route, kind string, at time.Time, result string, tier int) error {
	if db == nil || db.conn == nil || accountID <= 0 {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE bps_policy_blocks SET probe_count=probe_count+1, last_probe_result=$2, last_probe_at=$3,
			tier=CASE WHEN tier < $4 THEN $4 ELSE tier END WHERE account_id=$1 AND route=$5 AND kind=$6 AND cleared_at IS NULL`, accountID, clampUsageLogText(result, 120), at.Unix(), tier, route, kind)
		return err
	})
}

// ClearBPSPolicyBlock closes the account's active block at clearedAt and
// returns how long it lasted (false when none was active).
func (db *DB) ClearBPSPolicyBlock(ctx context.Context, accountID int64, clearedAt time.Time) (time.Duration, bool, error) {
	return db.ClearRouteBlock(ctx, accountID, BPSRouteBPS, RouteBlockPolicy, clearedAt)
}

// ClearRouteBlock is ClearBPSPolicyBlock for one route of the account.
func (db *DB) ClearRouteBlock(ctx context.Context, accountID int64, route, kind string, clearedAt time.Time) (time.Duration, bool, error) {
	if db == nil || db.conn == nil || accountID <= 0 {
		return 0, false, nil
	}
	var duration time.Duration
	var found bool
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var id, blockedAt int64
		err := tx.QueryRowContext(ctx, `SELECT id, blocked_at FROM bps_policy_blocks WHERE account_id=$1 AND route=$2 AND kind=$3 AND cleared_at IS NULL ORDER BY id DESC LIMIT 1`, accountID, route, kind).Scan(&id, &blockedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		seconds := max(clearedAt.Unix()-blockedAt, 0)
		duration, found = time.Duration(seconds)*time.Second, true
		_, err = tx.ExecContext(ctx, `UPDATE bps_policy_blocks SET cleared_at=$2, duration_seconds=$3 WHERE account_id=$1 AND route=$4 AND kind=$5 AND cleared_at IS NULL`, accountID, clearedAt.Unix(), seconds, route, kind)
		return err
	})
	return duration, found, err
}

// ListBPSPolicyBlocks returns active blocks and the most recent limit
// cleared ones, newest first.
func (db *DB) ListBPSPolicyBlocks(ctx context.Context, limit int) (active, history []BPSPolicyBlock, err error) {
	if db == nil || db.conn == nil {
		return nil, nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	scan := func(query string, args ...any) ([]BPSPolicyBlock, error) {
		rows, err := db.conn.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []BPSPolicyBlock{}
		for rows.Next() {
			var b BPSPolicyBlock
			var blockedAt, lastProbe int64
			var clearedAt sql.NullInt64
			if err := rows.Scan(&b.ID, &b.AccountID, &blockedAt, &b.Tier, &clearedAt, &b.DurationSeconds, &b.ProbeCount, &b.LastProbeResult, &lastProbe, &b.Route, &b.Detail, &b.Kind); err != nil {
				return nil, err
			}
			b.BlockedAt = time.Unix(blockedAt, 0).UTC()
			if clearedAt.Valid {
				t := time.Unix(clearedAt.Int64, 0).UTC()
				b.ClearedAt = &t
			}
			if lastProbe > 0 {
				t := time.Unix(lastProbe, 0).UTC()
				b.LastProbeAt = &t
			}
			out = append(out, b)
		}
		return out, rows.Err()
	}
	const columns = `SELECT id, account_id, blocked_at, tier, cleared_at, duration_seconds, probe_count, last_probe_result, last_probe_at, route, detail, kind FROM bps_policy_blocks`
	if active, err = scan(columns + ` WHERE cleared_at IS NULL ORDER BY blocked_at`); err != nil {
		return nil, nil, err
	}
	history, err = scan(columns+` WHERE cleared_at IS NOT NULL ORDER BY cleared_at DESC, id DESC LIMIT $1`, limit)
	return active, history, err
}

// BPSPolicyBlockTotals summarizes one account's blocks in the kept history.
type BPSPolicyBlockTotals struct {
	AccountID           int64  `json:"account_id"`
	Route               string `json:"route"`
	TimesBlocked        int    `json:"times_blocked"`
	TotalBlockedSeconds int64  `json:"total_blocked_seconds"`
	LongestBlockSeconds int64  `json:"longest_block_seconds"`
	Recovered           int    `json:"recovered"`
}

// BPSPolicyBlockTotalsAt sums every kept block per account; an active block
// counts its elapsed time until now.
func (db *DB) BPSPolicyBlockTotalsAt(ctx context.Context, now time.Time) ([]BPSPolicyBlockTotals, error) {
	if db == nil || db.conn == nil {
		return nil, nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id, COUNT(*),
			SUM(CASE WHEN cleared_at IS NULL THEN $1 - blocked_at ELSE duration_seconds END),
			MAX(CASE WHEN cleared_at IS NULL THEN $1 - blocked_at ELSE duration_seconds END),
			SUM(CASE WHEN cleared_at IS NULL THEN 0 ELSE 1 END)
		, route FROM bps_policy_blocks GROUP BY account_id, route ORDER BY account_id, route`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BPSPolicyBlockTotals{}
	for rows.Next() {
		var t BPSPolicyBlockTotals
		if err := rows.Scan(&t.AccountID, &t.TimesBlocked, &t.TotalBlockedSeconds, &t.LongestBlockSeconds, &t.Recovered, &t.Route); err != nil {
			return nil, err
		}
		t.TotalBlockedSeconds, t.LongestBlockSeconds = max(t.TotalBlockedSeconds, 0), max(t.LongestBlockSeconds, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// PruneBPSPolicyBlocks deletes cleared blocks that ended before cutoff.
func (db *DB) PruneBPSPolicyBlocks(ctx context.Context, cutoff time.Time) (int64, error) {
	if db == nil || db.conn == nil {
		return 0, nil
	}
	var affected int64
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM bps_policy_blocks WHERE cleared_at IS NOT NULL AND cleared_at < $1`, cutoff.Unix())
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}
