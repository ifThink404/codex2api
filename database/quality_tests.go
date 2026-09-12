package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const QualityTestConcurrency = 3

var ErrQualityTestCapacity = errors.New("最多同时运行 3 个检测任务，请等待一个任务完成")
var ErrQualityTestAccountBusy = errors.New("该账号已有进行中的检测任务")

type QualityTestMetrics struct {
	ResponseModel   string `json:"response_model,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	FirstContentMS  *int64 `json:"first_content_ms,omitempty"`
	InputTokens     *int64 `json:"input_tokens,omitempty"`
	OutputTokens    *int64 `json:"output_tokens,omitempty"`
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// Account identity is a snapshot, so renaming/deleting an account cannot rewrite history.
type QualityTestJob struct {
	ID              int64      `json:"id"`
	AccountID       int64      `json:"account_id"`
	AccountName     string     `json:"account_name"`
	PlanType        string     `json:"plan_type"`
	Channel         string     `json:"channel"`
	Model           string     `json:"model"`
	ReasoningEffort string     `json:"reasoning_effort"`
	Prompt          string     `json:"prompt,omitempty"`
	Status          string     `json:"status"`
	Output          string     `json:"output,omitempty"`
	Error           string     `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	DeadlineAt      time.Time  `json:"-"`
	QualityTestMetrics
}

type QualityTestPage struct {
	Jobs       []QualityTestJob `json:"jobs"`
	ActiveJobs []QualityTestJob `json:"active_jobs"`
	Total      int              `json:"total"`
	Limit      int              `json:"concurrency_limit"`
}

func (db *DB) ensureQualityTestSchema(ctx context.Context) error {
	idType, timeType := "BIGSERIAL PRIMARY KEY", "TIMESTAMPTZ"
	if db.isSQLite() {
		idType, timeType = "INTEGER PRIMARY KEY AUTOINCREMENT", "TIMESTAMP"
	}
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS quality_test_jobs (
		 id %s, slot INTEGER UNIQUE CHECK(slot BETWEEN 1 AND 3),
		 account_id BIGINT NOT NULL, account_name TEXT NOT NULL, plan_type TEXT NOT NULL,
		 channel TEXT NOT NULL, model TEXT NOT NULL, reasoning_effort TEXT NOT NULL,
		 prompt TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'running', output TEXT NOT NULL DEFAULT '',
		 metrics_json TEXT NOT NULL DEFAULT '{}', error TEXT NOT NULL DEFAULT '',
		 created_at %s NOT NULL, updated_at %s NOT NULL, deadline_at %s NOT NULL, completed_at %s,
		 CHECK ((slot IS NOT NULL AND status IN ('running','cancelling')) OR (slot IS NULL AND status IN ('completed','error','stopped','interrupted')))
		)`, idType, timeType, timeType, timeType, timeType),
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_quality_test_active_account ON quality_test_jobs(account_id) WHERE slot IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_quality_test_created ON quality_test_jobs(id DESC)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// Nullable UNIQUE slots enforce the limit across processes and replicas, not just browsers.
// A failed/conflicting insert never creates a record or starts upstream work.
func (db *DB) CreateQualityTestJob(ctx context.Context, job QualityTestJob) (*QualityTestJob, error) {
	if err := db.ExpireQualityTests(ctx, time.Now()); err != nil {
		return nil, err
	}
	query := `INSERT INTO quality_test_jobs(slot,account_id,account_name,plan_type,channel,model,reasoning_effort,prompt,created_at,updated_at,deadline_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10) ON CONFLICT DO NOTHING`
	now := time.Now().UTC()
	for slot := 1; slot <= QualityTestConcurrency; slot++ {
		var id int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			var err error
			id, err = db.insertRowID(ctx, query+" RETURNING id", query, slot, job.AccountID, job.AccountName, job.PlanType, job.Channel, job.Model, job.ReasoningEffort, job.Prompt, db.timeArg(now), db.timeArg(now.Add(10*time.Minute)))
			return err
		})
		if err == nil {
			job.ID, job.Status, job.CreatedAt, job.UpdatedAt, job.DeadlineAt = id, "running", now, now, now.Add(10*time.Minute)
			return &job, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	var busy bool
	if err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quality_test_jobs WHERE account_id=$1 AND slot IS NOT NULL)`, job.AccountID).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, ErrQualityTestAccountBusy
	}
	return nil, ErrQualityTestCapacity
}

// A crashed process cannot leave permanent occupied slots. The grace period is longer
// than a live worker's hard deadline; records are failed, never replayed automatically.
func (db *DB) ExpireQualityTests(ctx context.Context, now time.Time) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE quality_test_jobs SET status='interrupted',slot=NULL,error='检测进程中断或超时，请重新发起检测',completed_at=$1,updated_at=$1 WHERE slot IS NOT NULL AND deadline_at<$2`, db.timeArg(now), db.timeArg(now.Add(-30*time.Second)))
	return err
}

const qualityTestColumns = `id,account_id,account_name,plan_type,channel,model,reasoning_effort,status,metrics_json,error,created_at,updated_at,completed_at,deadline_at`

func scanQualityTestJob(scanner interface{ Scan(...any) error }, detail bool) (*QualityTestJob, error) {
	var job QualityTestJob
	var metrics string
	var created, updated, completed, deadline any
	args := []any{&job.ID, &job.AccountID, &job.AccountName, &job.PlanType, &job.Channel, &job.Model, &job.ReasoningEffort, &job.Status, &metrics, &job.Error, &created, &updated, &completed, &deadline}
	if detail {
		args = append(args, &job.Prompt, &job.Output)
	}
	if err := scanner.Scan(args...); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(metrics), &job.QualityTestMetrics); err != nil {
		return nil, err
	}
	var err error
	if job.CreatedAt, err = parseDBTimeValue(created); err != nil {
		return nil, err
	}
	if job.UpdatedAt, err = parseDBTimeValue(updated); err != nil {
		return nil, err
	}
	if job.DeadlineAt, err = parseDBTimeValue(deadline); err != nil {
		return nil, err
	}
	value, err := parseDBNullTimeValue(completed)
	if err != nil {
		return nil, err
	}
	if value.Valid {
		job.CompletedAt = &value.Time
	}
	if job.Status == "running" || job.Status == "cancelling" {
		job.DurationMS = max(0, time.Since(job.CreatedAt).Milliseconds())
	}
	return &job, nil
}

func (db *DB) GetQualityTestJob(ctx context.Context, id int64) (*QualityTestJob, error) {
	return scanQualityTestJob(db.conn.QueryRowContext(ctx, `SELECT `+qualityTestColumns+`,prompt,output FROM quality_test_jobs WHERE id=$1`, id), true)
}

func (db *DB) ListQualityTests(ctx context.Context, page, pageSize int) (*QualityTestPage, error) {
	if err := db.ExpireQualityTests(ctx, time.Now()); err != nil {
		return nil, err
	}
	page, pageSize = normalizePage(page, pageSize)
	result := &QualityTestPage{Jobs: []QualityTestJob{}, ActiveJobs: []QualityTestJob{}, Limit: QualityTestConcurrency}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM quality_test_jobs`).Scan(&result.Total); err != nil {
		return nil, err
	}
	read := func(query string, args ...any) ([]QualityTestJob, error) {
		rows, err := db.conn.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		jobs := []QualityTestJob{}
		for rows.Next() {
			job, err := scanQualityTestJob(rows, false)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, *job)
		}
		return jobs, rows.Err()
	}
	var err error
	result.Jobs, err = read(`SELECT `+qualityTestColumns+` FROM quality_test_jobs ORDER BY id DESC LIMIT $1 OFFSET $2`, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, err
	}
	result.ActiveJobs, err = read(`SELECT ` + qualityTestColumns + ` FROM quality_test_jobs WHERE slot IS NOT NULL ORDER BY id DESC`)
	return result, err
}

func (db *DB) QualityTestStatus(ctx context.Context, id int64) (string, error) {
	var status string
	err := db.conn.QueryRowContext(ctx, `SELECT status FROM quality_test_jobs WHERE id=$1`, id).Scan(&status)
	return status, err
}

func (db *DB) SaveQualityTestProgress(ctx context.Context, job QualityTestJob) error {
	metrics, err := json.Marshal(job.QualityTestMetrics)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `UPDATE quality_test_jobs SET output=$1,metrics_json=$2,updated_at=$3 WHERE id=$4 AND slot IS NOT NULL`, job.Output, string(metrics), db.timeArg(time.Now()), job.ID)
	return err
}

func (db *DB) FinishQualityTest(ctx context.Context, job QualityTestJob) error {
	metrics, err := json.Marshal(job.QualityTestMetrics)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `UPDATE quality_test_jobs SET status=CASE WHEN status='cancelling' THEN 'stopped' ELSE $1 END,slot=NULL,output=$2,metrics_json=$3,error=CASE WHEN status='cancelling' THEN '' ELSE $4 END,updated_at=$5,completed_at=$5 WHERE id=$6 AND slot IS NOT NULL`, job.Status, job.Output, string(metrics), job.Error, db.timeArg(time.Now()), job.ID)
	return err
}

func (db *DB) CancelQualityTest(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE quality_test_jobs SET status='cancelling',updated_at=$1 WHERE id=$2 AND status='running'`, db.timeArg(time.Now()), id)
	return err
}
