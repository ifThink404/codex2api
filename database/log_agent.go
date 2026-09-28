package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/internal/logagent"
)

// 日志分析 Agent 的配置与分析记录。两张表都由本文件按需建表（不进 migrate 主流程），
// 与插件分支合并时不会和 postgres.go / sqlite.go 的 DDL 冲突。
//
//   log_agent_settings：单行（id=1）JSON 配置。
//   log_agent_runs：每次分析一行，按 retention_days 清理。

const (
	DefaultLogAgentTimeoutSeconds = 90
	MinLogAgentTimeoutSeconds     = 10
	MaxLogAgentTimeoutSeconds     = 300
	DefaultLogAgentRetentionDays  = 30
	MaxLogAgentRetentionDays      = 365

	LogAgentRunStatusSucceeded = "succeeded"
	LogAgentRunStatusFallback  = "fallback"
	LogAgentRunStatusFailed    = "failed"

	logAgentPurgeBatch = 1000
)

// LogAgentConfig 是管理员选择的分析通道与上下文上限。APIKeyID=0 表示不绑定网关 Key
// （与提示词情报的号池分析一致，按内部请求路由）。
type LogAgentConfig struct {
	Enabled        bool   `json:"enabled"`
	APIKeyID       int64  `json:"api_key_id"`
	Model          string `json:"model"`
	MaxInputBytes  int    `json:"max_input_bytes"`
	MaxRecords     int    `json:"max_records"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	RetentionDays  int    `json:"retention_days"`
}

// DefaultLogAgentConfig 默认关闭：分析会消耗号池额度，必须由管理员显式开启。
func DefaultLogAgentConfig() LogAgentConfig {
	return LogAgentConfig{
		MaxInputBytes:  logagent.DefaultMaxInputBytes,
		MaxRecords:     logagent.DefaultMaxRecords,
		TimeoutSeconds: DefaultLogAgentTimeoutSeconds,
		RetentionDays:  DefaultLogAgentRetentionDays,
	}
}

// Normalize 把缺省值补齐、越界值钳到边界。
func (c LogAgentConfig) Normalize() LogAgentConfig {
	defaults := DefaultLogAgentConfig()
	c.Model = strings.TrimSpace(c.Model)
	if len(c.Model) > 128 {
		c.Model = c.Model[:128]
	}
	if c.APIKeyID < 0 {
		c.APIKeyID = 0
	}
	limits := logagent.Limits{MaxInputBytes: c.MaxInputBytes, MaxRecords: c.MaxRecords}.Normalize()
	c.MaxInputBytes, c.MaxRecords = limits.MaxInputBytes, limits.MaxRecords
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = defaults.TimeoutSeconds
	}
	c.TimeoutSeconds = min(max(c.TimeoutSeconds, MinLogAgentTimeoutSeconds), MaxLogAgentTimeoutSeconds)
	if c.RetentionDays <= 0 {
		c.RetentionDays = defaults.RetentionDays
	}
	c.RetentionDays = min(c.RetentionDays, MaxLogAgentRetentionDays)
	return c
}

// LogAgentRun 是一次分析的持久化记录。Subject/Findings/ContextStats 为 JSON 原文，
// 由调用方（admin）定义结构，数据库层只负责存取。
type LogAgentRun struct {
	ID           int64           `json:"id"`
	Source       string          `json:"source"`
	Subject      json.RawMessage `json:"subject"`
	Model        string          `json:"model"`
	APIKeyID     int64           `json:"api_key_id"`
	Status       string          `json:"status"`
	Findings     json.RawMessage `json:"findings"`
	ContextStats json.RawMessage `json:"context_stats"`
	ErrorMessage string          `json:"error_message"`
	RecordCount  int             `json:"record_count"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	TotalTokens  int             `json:"total_tokens"`
	DurationMs   int64           `json:"duration_ms"`
	CreatedAt    time.Time       `json:"created_at"`
}

// LogAgentRunFilter 是分析记录列表的筛选：Source 精确匹配，BeforeID>0 时向更早翻页。
type LogAgentRunFilter struct {
	Source   string
	BeforeID int64
	Limit    int
}

var (
	logAgentSchemaMu    sync.Mutex
	logAgentSchemaReady = make(map[*DB]bool)
)

func (db *DB) ensureLogAgentSchema(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return errors.New("database unavailable")
	}
	logAgentSchemaMu.Lock()
	defer logAgentSchemaMu.Unlock()
	if logAgentSchemaReady[db] {
		return nil
	}
	idType, timeType := "BIGSERIAL PRIMARY KEY", "TIMESTAMPTZ"
	if db.isSQLite() {
		idType, timeType = "INTEGER PRIMARY KEY AUTOINCREMENT", "TIMESTAMP"
	}
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS log_agent_settings (
			id INTEGER PRIMARY KEY,
			config TEXT NOT NULL DEFAULT '{}',
			updated_at %s NOT NULL
		)`, timeType),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS log_agent_runs (
			id %s,
			source VARCHAR(64) NOT NULL,
			subject TEXT NOT NULL DEFAULT '{}',
			model VARCHAR(128) NOT NULL DEFAULT '',
			api_key_id BIGINT NOT NULL DEFAULT 0,
			status VARCHAR(16) NOT NULL,
			findings TEXT NOT NULL DEFAULT '{}',
			context_stats TEXT NOT NULL DEFAULT '{}',
			error_message TEXT NOT NULL DEFAULT '',
			record_count INT NOT NULL DEFAULT 0,
			input_tokens INT NOT NULL DEFAULT 0,
			output_tokens INT NOT NULL DEFAULT 0,
			total_tokens INT NOT NULL DEFAULT 0,
			duration_ms BIGINT NOT NULL DEFAULT 0,
			created_at %s NOT NULL
		)`, idType, timeType),
		`CREATE INDEX IF NOT EXISTS idx_log_agent_runs_created_at ON log_agent_runs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_log_agent_runs_source_id ON log_agent_runs(source, id)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	logAgentSchemaReady[db] = true
	return nil
}

// LoadLogAgentConfig 读取配置；未配置或 JSON 损坏时返回默认值。
func (db *DB) LoadLogAgentConfig(ctx context.Context) (LogAgentConfig, error) {
	if err := db.ensureLogAgentSchema(ctx); err != nil {
		return DefaultLogAgentConfig(), err
	}
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT config FROM log_agent_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultLogAgentConfig(), nil
	}
	if err != nil {
		return DefaultLogAgentConfig(), err
	}
	cfg := DefaultLogAgentConfig()
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return DefaultLogAgentConfig(), nil
	}
	return cfg.Normalize(), nil
}

// SaveLogAgentConfig 规范化后持久化，返回实际落库的配置。
func (db *DB) SaveLogAgentConfig(ctx context.Context, cfg LogAgentConfig) (LogAgentConfig, error) {
	cfg = cfg.Normalize()
	if err := db.ensureLogAgentSchema(ctx); err != nil {
		return cfg, err
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	err = db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO log_agent_settings (id, config, updated_at) VALUES (1, $1, $2)
			ON CONFLICT (id) DO UPDATE SET config = excluded.config, updated_at = excluded.updated_at
		`, string(payload), db.timeArg(time.Now().UTC()))
		return err
	})
	return cfg, err
}

func logAgentJSONText(raw json.RawMessage) string {
	if len(raw) == 0 || !json.Valid(raw) {
		return "{}"
	}
	return string(raw)
}

// InsertLogAgentRun 写入一条分析记录并回填 ID 与创建时间。
func (db *DB) InsertLogAgentRun(ctx context.Context, run *LogAgentRun) error {
	if run == nil {
		return errors.New("log agent run is nil")
	}
	if err := db.ensureLogAgentSchema(ctx); err != nil {
		return err
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	args := []any{
		clampUsageLogText(run.Source, 64), logAgentJSONText(run.Subject), clampUsageLogText(run.Model, 128), run.APIKeyID,
		clampUsageLogText(run.Status, 16), logAgentJSONText(run.Findings), logAgentJSONText(run.ContextStats),
		clampUsageLogText(run.ErrorMessage, 2000), run.RecordCount, run.InputTokens, run.OutputTokens, run.TotalTokens,
		run.DurationMs, db.timeArg(run.CreatedAt),
	}
	const columns = `source, subject, model, api_key_id, status, findings, context_stats, error_message, record_count, input_tokens, output_tokens, total_tokens, duration_ms, created_at`
	const values = `$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14`
	return db.withSQLiteWriteLock(ctx, func() error {
		if db.isSQLite() {
			result, err := db.conn.ExecContext(ctx, `INSERT INTO log_agent_runs (`+columns+`) VALUES (`+values+`)`, args...)
			if err != nil {
				return err
			}
			run.ID, err = result.LastInsertId()
			return err
		}
		return db.conn.QueryRowContext(ctx, `INSERT INTO log_agent_runs (`+columns+`) VALUES (`+values+`) RETURNING id`, args...).Scan(&run.ID)
	})
}

const logAgentRunSelect = `SELECT id, source, subject, model, api_key_id, status, findings, context_stats, error_message, record_count, input_tokens, output_tokens, total_tokens, duration_ms, created_at FROM log_agent_runs`

func scanLogAgentRun(scanner interface{ Scan(...any) error }) (*LogAgentRun, error) {
	run := &LogAgentRun{}
	var subject, findings, stats string
	var createdRaw any
	if err := scanner.Scan(&run.ID, &run.Source, &subject, &run.Model, &run.APIKeyID, &run.Status, &findings, &stats,
		&run.ErrorMessage, &run.RecordCount, &run.InputTokens, &run.OutputTokens, &run.TotalTokens, &run.DurationMs, &createdRaw); err != nil {
		return nil, err
	}
	run.Subject, run.Findings, run.ContextStats = json.RawMessage(subject), json.RawMessage(findings), json.RawMessage(stats)
	var err error
	if run.CreatedAt, err = parseDBTimeValue(createdRaw); err != nil {
		return nil, err
	}
	return run, nil
}

// GetLogAgentRun 按 ID 读取；不存在时返回 sql.ErrNoRows。
func (db *DB) GetLogAgentRun(ctx context.Context, id int64) (*LogAgentRun, error) {
	if err := db.ensureLogAgentSchema(ctx); err != nil {
		return nil, err
	}
	return scanLogAgentRun(db.conn.QueryRowContext(ctx, logAgentRunSelect+` WHERE id = $1`, id))
}

// ListLogAgentRuns 按 ID 倒序列出分析记录。
func (db *DB) ListLogAgentRuns(ctx context.Context, filter LogAgentRunFilter) ([]*LogAgentRun, error) {
	if err := db.ensureLogAgentSchema(ctx); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var where []string
	var args []any
	if source := strings.TrimSpace(filter.Source); source != "" {
		args = append(args, source)
		where = append(where, fmt.Sprintf("source = $%d", len(args)))
	}
	if filter.BeforeID > 0 {
		args = append(args, filter.BeforeID)
		where = append(where, fmt.Sprintf("id < $%d", len(args)))
	}
	query := logAgentRunSelect
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []*LogAgentRun{}
	for rows.Next() {
		run, err := scanLogAgentRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// PurgeLogAgentRuns 分批删除 cutoff 之前的分析记录，返回删除行数。
func (db *DB) PurgeLogAgentRuns(ctx context.Context, cutoff time.Time) (int64, error) {
	if err := db.ensureLogAgentSchema(ctx); err != nil {
		return 0, err
	}
	var total int64
	for {
		var affected int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			result, err := db.conn.ExecContext(ctx, `
				DELETE FROM log_agent_runs WHERE id IN (
					SELECT id FROM log_agent_runs WHERE created_at < $1 ORDER BY id LIMIT $2
				)`, db.timeArg(cutoff.UTC()), logAgentPurgeBatch)
			if err != nil {
				return err
			}
			affected, err = result.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += affected
		if affected < logAgentPurgeBatch || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}
