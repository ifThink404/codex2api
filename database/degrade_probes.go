package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Degradation ("降智") judge probes: one row per pelican probe of an account's
// route (native or bps), with the extracted HTML sample for review. Rows are
// kept DegradeProbeRetention.

const (
	DegradeProbeRetention = 7 * 24 * time.Hour
	// DegradeProbeHTMLLimit caps the stored sample.
	DegradeProbeHTMLLimit = 256 << 10
)

func (db *DB) migrateDegradeProbes(ctx context.Context) error {
	id := "BIGSERIAL PRIMARY KEY"
	if db.isSQLite() {
		id = "INTEGER PRIMARY KEY AUTOINCREMENT"
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS degrade_probes (
			id ` + id + `,
			account_id BIGINT NOT NULL,
			route TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			score INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0,
			verdict TEXT NOT NULL,
			probe_trigger TEXT NOT NULL DEFAULT '',
			html TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			duration_ms BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_degrade_probes_account ON degrade_probes(account_id, route, id)`,
		`CREATE INDEX IF NOT EXISTS idx_degrade_probes_created ON degrade_probes(created_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// DegradeProbe is one pelican probe. HTML is only filled by GetDegradeProbe.
type DegradeProbe struct {
	ID            int64     `json:"id"`
	AccountID     int64     `json:"account_id"`
	Route         string    `json:"route"`
	Model         string    `json:"model"`
	UpstreamModel string    `json:"upstream_model,omitempty"`
	Score         int       `json:"score"`
	Bytes         int       `json:"bytes"`
	Verdict       string    `json:"verdict"`
	Trigger       string    `json:"trigger"`
	HTML          string    `json:"html,omitempty"`
	Error         string    `json:"error,omitempty"`
	DurationMs    int64     `json:"duration_ms"`
	CreatedAt     time.Time `json:"created_at"`
}

// InsertDegradeProbe stores a probe (the sample capped at 256 KB).
func (db *DB) InsertDegradeProbe(ctx context.Context, p *DegradeProbe) error {
	if db == nil || db.conn == nil || p == nil {
		return nil
	}
	html := p.HTML
	if len(html) > DegradeProbeHTMLLimit {
		html = strings.ToValidUTF8(html[:DegradeProbeHTMLLimit], "")
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `INSERT INTO degrade_probes(account_id,route,model,upstream_model,score,bytes,verdict,probe_trigger,html,error,duration_ms,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
		args := []any{p.AccountID, p.Route, clampUsageLogText(p.Model, 120), clampUsageLogText(p.UpstreamModel, 120), p.Score, p.Bytes, p.Verdict, p.Trigger, html, clampUsageLogText(p.Error, 300), p.DurationMs, p.CreatedAt.Unix()}
		if db.isSQLite() {
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			p.ID, err = res.LastInsertId()
			return err
		}
		return tx.QueryRowContext(ctx, query+` RETURNING id`, args...).Scan(&p.ID)
	})
}

// DegradeProbeFilter narrows ListDegradeProbes.
type DegradeProbeFilter struct {
	AccountID int64
	Route     string
	Verdict   string
	Limit     int
}

const degradeProbeColumns = `id, account_id, route, model, upstream_model, score, bytes, verdict, probe_trigger, error, duration_ms, created_at`

func scanDegradeProbe(scan func(...any) error, withHTML bool) (DegradeProbe, error) {
	var p DegradeProbe
	var created int64
	dest := []any{&p.ID, &p.AccountID, &p.Route, &p.Model, &p.UpstreamModel, &p.Score, &p.Bytes, &p.Verdict, &p.Trigger, &p.Error, &p.DurationMs, &created}
	if withHTML {
		dest = append(dest, &p.HTML)
	}
	if err := scan(dest...); err != nil {
		return p, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	return p, nil
}

// ListDegradeProbes returns probes newest first, without their samples.
func (db *DB) ListDegradeProbes(ctx context.Context, f DegradeProbeFilter) ([]DegradeProbe, error) {
	out := []DegradeProbe{}
	if db == nil || db.conn == nil {
		return out, nil
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	where, args := []string{"1=1"}, []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.Replace(clause, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if f.AccountID > 0 {
		add("account_id = ?", f.AccountID)
	}
	if f.Route != "" {
		add("route = ?", f.Route)
	}
	if f.Verdict != "" {
		add("verdict = ?", f.Verdict)
	}
	args = append(args, f.Limit)
	rows, err := db.conn.QueryContext(ctx, `SELECT `+degradeProbeColumns+` FROM degrade_probes WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanDegradeProbe(rows.Scan, false)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetDegradeProbe returns one probe with its sample (nil when missing).
func (db *DB) GetDegradeProbe(ctx context.Context, id int64) (*DegradeProbe, error) {
	if db == nil || db.conn == nil {
		return nil, nil
	}
	row := db.conn.QueryRowContext(ctx, `SELECT `+degradeProbeColumns+`, html FROM degrade_probes WHERE id = $1`, id)
	p, err := scanDegradeProbe(row.Scan, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// LatestDegradeProbes returns the newest probe of every account and route.
func (db *DB) LatestDegradeProbes(ctx context.Context) ([]DegradeProbe, error) {
	out := []DegradeProbe{}
	if db == nil || db.conn == nil {
		return out, nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT `+degradeProbeColumns+` FROM degrade_probes WHERE id IN (SELECT MAX(id) FROM degrade_probes GROUP BY account_id, route)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanDegradeProbe(rows.Scan, false)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneDegradeProbes deletes probes created before cutoff.
func (db *DB) PruneDegradeProbes(ctx context.Context, cutoff time.Time) (int64, error) {
	if db == nil || db.conn == nil {
		return 0, nil
	}
	var affected int64
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM degrade_probes WHERE created_at < $1`, cutoff.Unix())
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	return affected, err
}
