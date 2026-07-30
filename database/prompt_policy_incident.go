package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	PromptPolicyEvaluationCompleted     = "completed"
	PromptPolicyEvaluationNotRun        = "not_run"
	PromptPolicyEvaluationUnavailable   = "unavailable"
	PromptPolicyEvaluationLegacyUnknown = "legacy_unknown"

	PromptPolicyOutcomeNoHit    = "no_hit"
	PromptPolicyOutcomeAuditHit = "audit_hit"
	PromptPolicyOutcomeWarn     = "warn"
	PromptPolicyOutcomeBlock    = "block"
)

type PromptPolicyIncident struct {
	ID                   int64     `json:"id"`
	IncidentID           string    `json:"incident_id"`
	RequestCorrelationID string    `json:"request_correlation_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	AttemptIndex         int       `json:"attempt_index"`
	Transport            string    `json:"transport"`
	Endpoint             string    `json:"endpoint"`
	Protocol             string    `json:"protocol"`
	Provider             string    `json:"provider"`
	Model                string    `json:"model"`
	StatusCode           int       `json:"status_code"`
	AccountID            int64     `json:"account_id"`
	APIKeyID             int64     `json:"api_key_id"`
	APIKeyName           string    `json:"api_key_name"`
	APIKeyMasked         string    `json:"api_key_masked"`
	Platform             string    `json:"platform"`
	SourceRef            string    `json:"source_ref,omitempty"`
	UpstreamErrorCode    string    `json:"upstream_error_code"`
	UpstreamError        string    `json:"upstream_error"`
	LocalEvaluationState string    `json:"local_evaluation_state"`
	LocalOutcome         string    `json:"local_outcome"`
	LocalAction          string    `json:"local_action"`
	LocalScore           *int      `json:"local_score"`
	LocalRawScore        *int      `json:"local_raw_score"`
	LocalAuditScore      *int      `json:"local_audit_score"`
	LocalAuditRawScore   *int      `json:"local_audit_raw_score"`
	LocalThreshold       int       `json:"local_threshold"`
	LocalMode            string    `json:"local_mode"`
	LocalPolicyProfile   string    `json:"local_policy_profile"`
	LocalReasonCode      string    `json:"local_reason_code"`
	LocalReason          string    `json:"local_reason"`
	LocalPrimaryOrigin   string    `json:"local_primary_origin"`
	LocalStrikeEligible  bool      `json:"local_strike_eligible"`
	LocalReviewModel     string    `json:"local_review_model"`
	LocalReviewFlagged   bool      `json:"local_review_flagged"`
	LocalReviewError     string    `json:"local_review_error"`
	LocalMatchedPatterns string    `json:"local_matched_patterns"`
	PromptFingerprint    string    `json:"prompt_fingerprint"`
	PromptPreview        string    `json:"prompt_preview"`
	PromptText           string    `json:"prompt_text"`
	CandidateID          int64     `json:"candidate_id,omitempty"`
	CandidateEvidenceID  int64     `json:"candidate_evidence_id,omitempty"`
	LocalMiss            bool      `json:"local_miss"`
}

type PromptPolicyIncidentInput struct {
	IncidentID           string
	RequestCorrelationID string
	AttemptIndex         int
	Transport            string
	Endpoint             string
	Protocol             string
	Provider             string
	Model                string
	StatusCode           int
	AccountID            int64
	APIKeyID             int64
	APIKeyName           string
	APIKeyMasked         string
	Platform             string
	SourceRef            string
	UpstreamErrorCode    string
	UpstreamError        string
	LocalEvaluationState string
	LocalOutcome         string
	LocalAction          string
	LocalScore           *int
	LocalRawScore        *int
	LocalAuditScore      *int
	LocalAuditRawScore   *int
	LocalThreshold       int
	LocalMode            string
	LocalPolicyProfile   string
	LocalReasonCode      string
	LocalReason          string
	LocalPrimaryOrigin   string
	LocalStrikeEligible  bool
	LocalReviewModel     string
	LocalReviewFlagged   bool
	LocalReviewError     string
	LocalMatchedPatterns string
	PromptFingerprint    string
	PromptPreview        string
	PromptText           string
	ObservedAt           time.Time
}

type PromptPolicyIncidentQuery struct {
	Page            int
	PageSize        int
	Endpoint        string
	Model           string
	APIKeyID        int64
	EvaluationState string
	Outcome         string
	LocalMiss       *bool
	Query           string
}

const promptPolicyIncidentSelect = `SELECT id, incident_id, COALESCE(request_correlation_id, ''), created_at,
	COALESCE(attempt_index, 0), COALESCE(transport, ''), COALESCE(endpoint, ''), COALESCE(request_protocol, ''),
	COALESCE(request_provider, ''), COALESCE(model, ''), COALESCE(status_code, 0), COALESCE(account_id, 0),
	COALESCE(api_key_id, 0), COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), COALESCE(platform, ''),
	COALESCE(source_ref, ''), COALESCE(upstream_error_code, ''), COALESCE(upstream_error, ''),
	COALESCE(local_evaluation_state, ''), COALESCE(local_outcome, ''), COALESCE(local_action, ''),
	local_score, local_raw_score, local_audit_score, local_audit_raw_score, COALESCE(local_threshold, 0),
	COALESCE(local_mode, ''), COALESCE(local_policy_profile, ''), COALESCE(local_reason_code, ''), COALESCE(local_reason, ''),
	COALESCE(local_primary_origin, ''), COALESCE(local_strike_eligible, false), COALESCE(local_review_model, ''),
	COALESCE(local_review_flagged, false), COALESCE(local_review_error, ''), COALESCE(local_matched_patterns, '[]'),
	COALESCE(prompt_fingerprint, ''), COALESCE(prompt_preview, ''), COALESCE(prompt_text, ''),
	COALESCE(candidate_id, 0), COALESCE(candidate_evidence_id, 0) FROM prompt_policy_incidents`

func (db *DB) ensurePromptPolicyIncidentsTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database is nil")
	}
	if db.isSQLite() {
		if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_policy_incidents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			incident_id TEXT NOT NULL UNIQUE,
			request_correlation_id TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			attempt_index INTEGER DEFAULT 0,
			transport TEXT DEFAULT '', endpoint TEXT DEFAULT '', request_protocol TEXT DEFAULT '', request_provider TEXT DEFAULT '', model TEXT DEFAULT '',
			status_code INTEGER DEFAULT 0, account_id INTEGER DEFAULT 0, api_key_id INTEGER DEFAULT 0, api_key_name TEXT DEFAULT '', api_key_masked TEXT DEFAULT '', platform TEXT DEFAULT '', source_ref TEXT DEFAULT '',
			upstream_error_code TEXT DEFAULT '', upstream_error TEXT DEFAULT '',
			local_evaluation_state TEXT DEFAULT '', local_outcome TEXT DEFAULT '', local_action TEXT DEFAULT '',
			local_score INTEGER NULL, local_raw_score INTEGER NULL, local_audit_score INTEGER NULL, local_audit_raw_score INTEGER NULL,
			local_threshold INTEGER DEFAULT 0, local_mode TEXT DEFAULT '', local_policy_profile TEXT DEFAULT '', local_reason_code TEXT DEFAULT '', local_reason TEXT DEFAULT '', local_primary_origin TEXT DEFAULT '', local_strike_eligible INTEGER DEFAULT 0,
			local_review_model TEXT DEFAULT '', local_review_flagged INTEGER DEFAULT 0, local_review_error TEXT DEFAULT '', local_matched_patterns TEXT DEFAULT '[]',
			prompt_fingerprint TEXT DEFAULT '', prompt_preview TEXT DEFAULT '', prompt_text TEXT DEFAULT '', candidate_id INTEGER DEFAULT 0, candidate_evidence_id INTEGER DEFAULT 0
		)`); err != nil {
			return err
		}
		for _, column := range []struct{ table, name, def string }{
			{"usage_logs", "prompt_policy_incident_id", "TEXT NULL"},
			{"prompt_rule_candidate_evidence", "prompt_policy_incident_id", "TEXT NULL"},
			{"prompt_filter_logs", "request_correlation_id", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "local_reason", "TEXT DEFAULT ''"},
		} {
			if err := db.ensureSQLiteColumn(ctx, column.table, column.name, column.def); err != nil {
				return err
			}
		}
	} else {
		if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_policy_incidents (
			id BIGSERIAL PRIMARY KEY,
			incident_id VARCHAR(64) NOT NULL UNIQUE,
			request_correlation_id VARCHAR(64) DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			attempt_index INT DEFAULT 0,
			transport VARCHAR(32) DEFAULT '', endpoint VARCHAR(256) DEFAULT '', request_protocol VARCHAR(64) DEFAULT '', request_provider VARCHAR(64) DEFAULT '', model VARCHAR(100) DEFAULT '',
			status_code INT DEFAULT 0, account_id BIGINT DEFAULT 0, api_key_id BIGINT DEFAULT 0, api_key_name VARCHAR(255) DEFAULT '', api_key_masked VARCHAR(64) DEFAULT '', platform VARCHAR(100) DEFAULT '', source_ref TEXT DEFAULT '',
			upstream_error_code VARCHAR(100) DEFAULT '', upstream_error TEXT DEFAULT '',
			local_evaluation_state VARCHAR(32) DEFAULT '', local_outcome VARCHAR(32) DEFAULT '', local_action VARCHAR(32) DEFAULT '',
			local_score INT NULL, local_raw_score INT NULL, local_audit_score INT NULL, local_audit_raw_score INT NULL,
			local_threshold INT DEFAULT 0, local_mode VARCHAR(32) DEFAULT '', local_policy_profile VARCHAR(32) DEFAULT '', local_reason_code VARCHAR(100) DEFAULT '', local_reason TEXT DEFAULT '', local_primary_origin VARCHAR(64) DEFAULT '', local_strike_eligible BOOLEAN DEFAULT FALSE,
			local_review_model VARCHAR(100) DEFAULT '', local_review_flagged BOOLEAN DEFAULT FALSE, local_review_error TEXT DEFAULT '', local_matched_patterns TEXT DEFAULT '[]',
			prompt_fingerprint VARCHAR(64) DEFAULT '', prompt_preview TEXT DEFAULT '', prompt_text TEXT DEFAULT '', candidate_id BIGINT DEFAULT 0, candidate_evidence_id BIGINT DEFAULT 0
		);
		ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS prompt_policy_incident_id VARCHAR(64) NULL;
		ALTER TABLE prompt_rule_candidate_evidence ADD COLUMN IF NOT EXISTS prompt_policy_incident_id VARCHAR(64) NULL;
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS request_correlation_id VARCHAR(64) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS local_reason TEXT DEFAULT '';
		`); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_request ON prompt_policy_incidents(request_correlation_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_created ON prompt_policy_incidents(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_api_key ON prompt_policy_incidents(api_key_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_endpoint ON prompt_policy_incidents(endpoint, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_outcome ON prompt_policy_incidents(local_outcome, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_policy_incident ON usage_logs(prompt_policy_incident_id)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_rule_evidence_incident ON prompt_rule_candidate_evidence(prompt_policy_incident_id)`,
	} {
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return db.migrateLegacyPromptPolicyIncidents(ctx)
}

func (db *DB) migrateLegacyPromptPolicyIncidents(ctx context.Context) error {
	query := `INSERT INTO prompt_policy_incidents (
		incident_id, created_at, endpoint, request_protocol, request_provider, model, api_key_id, api_key_name, api_key_masked,
		upstream_error_code, upstream_error, local_evaluation_state, local_matched_patterns
	) SELECT 'legacy-' || CAST(id AS TEXT), created_at, endpoint, request_protocol, request_provider, model, api_key_id, api_key_name, api_key_masked,
		error_code, full_text, $1, '[]' FROM prompt_filter_logs WHERE source='upstream_cyber_policy'
	ON CONFLICT(incident_id) DO NOTHING`
	_, err := db.conn.ExecContext(ctx, query, PromptPolicyEvaluationLegacyUnknown)
	return err
}

func normalizePromptPolicyIncidentInput(input PromptPolicyIncidentInput) (PromptPolicyIncidentInput, error) {
	input.IncidentID = strings.TrimSpace(input.IncidentID)
	if input.IncidentID == "" {
		return input, errors.New("incident id is required")
	}
	input.RequestCorrelationID = truncateCandidateRunes(strings.TrimSpace(input.RequestCorrelationID), 64)
	input.Transport = truncateCandidateRunes(strings.TrimSpace(input.Transport), 32)
	input.Endpoint = truncateCandidateRunes(strings.TrimSpace(input.Endpoint), 256)
	input.Protocol = truncateCandidateRunes(strings.TrimSpace(input.Protocol), 64)
	input.Provider = truncateCandidateRunes(strings.TrimSpace(input.Provider), 64)
	input.Model = truncateCandidateRunes(strings.TrimSpace(input.Model), 100)
	input.APIKeyName = truncateCandidateRunes(strings.TrimSpace(input.APIKeyName), 255)
	input.APIKeyMasked = truncateCandidateRunes(strings.TrimSpace(input.APIKeyMasked), 64)
	input.Platform = truncateCandidateRunes(strings.TrimSpace(input.Platform), 100)
	input.SourceRef = truncateCandidateRunes(strings.TrimSpace(input.SourceRef), 2000)
	input.UpstreamErrorCode = truncateCandidateRunes(strings.TrimSpace(input.UpstreamErrorCode), 100)
	input.UpstreamError = truncateCandidateRunes(strings.TrimSpace(input.UpstreamError), 8192)
	input.LocalEvaluationState = truncateCandidateRunes(strings.TrimSpace(input.LocalEvaluationState), 32)
	input.LocalOutcome = truncateCandidateRunes(strings.TrimSpace(input.LocalOutcome), 32)
	input.LocalAction = truncateCandidateRunes(strings.TrimSpace(input.LocalAction), 32)
	input.LocalMode = truncateCandidateRunes(strings.TrimSpace(input.LocalMode), 32)
	input.LocalPolicyProfile = truncateCandidateRunes(strings.TrimSpace(input.LocalPolicyProfile), 32)
	input.LocalReasonCode = truncateCandidateRunes(strings.TrimSpace(input.LocalReasonCode), 100)
	input.LocalReason = truncateCandidateRunes(strings.TrimSpace(input.LocalReason), 2000)
	input.LocalPrimaryOrigin = truncateCandidateRunes(strings.TrimSpace(input.LocalPrimaryOrigin), 64)
	input.LocalReviewModel = truncateCandidateRunes(strings.TrimSpace(input.LocalReviewModel), 100)
	input.LocalReviewError = truncateCandidateRunes(strings.TrimSpace(input.LocalReviewError), 2000)
	input.LocalMatchedPatterns = truncateCandidateRunes(strings.TrimSpace(input.LocalMatchedPatterns), 16000)
	if input.LocalMatchedPatterns == "" {
		input.LocalMatchedPatterns = "[]"
	}
	input.PromptFingerprint = truncateCandidateRunes(strings.TrimSpace(input.PromptFingerprint), 64)
	input.PromptPreview = truncateCandidateRunes(strings.TrimSpace(input.PromptPreview), 2000)
	input.PromptText = truncateCandidateRunes(strings.TrimSpace(input.PromptText), 32000)
	if input.ObservedAt.IsZero() {
		input.ObservedAt = time.Now().UTC()
	}
	return input, nil
}

func (db *DB) PersistPromptPolicyIncident(ctx context.Context, rawIncident PromptPolicyIncidentInput, rawCandidate PromptRuleCandidateInput, rawEvidence PromptRuleCandidateEvidenceInput) error {
	if db == nil {
		return errors.New("database is nil")
	}
	incident, err := normalizePromptPolicyIncidentInput(rawIncident)
	if err != nil {
		return err
	}
	candidate, err := normalizePromptRuleCandidateInput(rawCandidate)
	if err != nil {
		return err
	}
	evidence, err := normalizePromptRuleCandidateEvidenceInput(rawEvidence)
	if err != nil {
		return err
	}
	evidence.PromptPolicyIncidentID = incident.IncidentID
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO prompt_policy_incidents (
			incident_id, request_correlation_id, created_at, attempt_index, transport, endpoint, request_protocol, request_provider, model,
			status_code, account_id, api_key_id, api_key_name, api_key_masked, platform, source_ref, upstream_error_code, upstream_error,
			local_evaluation_state, local_outcome, local_action, local_score, local_raw_score, local_audit_score, local_audit_raw_score,
			local_threshold, local_mode, local_policy_profile, local_reason_code, local_reason, local_primary_origin, local_strike_eligible,
			local_review_model, local_review_flagged, local_review_error, local_matched_patterns, prompt_fingerprint, prompt_preview, prompt_text
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39)
		ON CONFLICT(incident_id) DO NOTHING`, incident.IncidentID, incident.RequestCorrelationID, incident.ObservedAt, incident.AttemptIndex,
			incident.Transport, incident.Endpoint, incident.Protocol, incident.Provider, incident.Model, incident.StatusCode, incident.AccountID,
			incident.APIKeyID, incident.APIKeyName, incident.APIKeyMasked, incident.Platform, incident.SourceRef, incident.UpstreamErrorCode,
			incident.UpstreamError, incident.LocalEvaluationState, incident.LocalOutcome, incident.LocalAction, incident.LocalScore,
			incident.LocalRawScore, incident.LocalAuditScore, incident.LocalAuditRawScore, incident.LocalThreshold, incident.LocalMode,
			incident.LocalPolicyProfile, incident.LocalReasonCode, incident.LocalReason, incident.LocalPrimaryOrigin, incident.LocalStrikeEligible,
			incident.LocalReviewModel, incident.LocalReviewFlagged, incident.LocalReviewError, incident.LocalMatchedPatterns,
			incident.PromptFingerprint, incident.PromptPreview, incident.PromptText); execErr != nil {
			return execErr
		}
		candidateID, evidenceID, _, stageErr := stagePromptRuleCandidateTx(ctx, tx, candidate, evidence)
		if stageErr != nil {
			return stageErr
		}
		if _, execErr := tx.ExecContext(ctx, `UPDATE prompt_policy_incidents SET candidate_id=$1, candidate_evidence_id=$2 WHERE incident_id=$3`, candidateID, evidenceID, incident.IncidentID); execErr != nil {
			return execErr
		}
		return tx.Commit()
	})
}

func (db *DB) GetPromptPolicyIncident(ctx context.Context, incidentID string) (*PromptPolicyIncident, error) {
	row := db.conn.QueryRowContext(ctx, promptPolicyIncidentSelect+` WHERE incident_id=$1`, strings.TrimSpace(incidentID))
	return scanPromptPolicyIncident(row)
}

func (db *DB) ListPromptPolicyIncidentsPage(ctx context.Context, query PromptPolicyIncidentQuery) ([]*PromptPolicyIncident, int, error) {
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 || query.PageSize > 500 {
		query.PageSize = 20
	}
	where, args := promptPolicyIncidentWhere(query)
	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := db.conn.QueryContext(ctx, promptPolicyIncidentSelect+where+` ORDER BY id DESC LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]*PromptPolicyIncident, 0)
	for rows.Next() {
		item, scanErr := scanPromptPolicyIncident(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func promptPolicyIncidentWhere(query PromptPolicyIncidentQuery) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)
	addExact := func(column, value string) {
		if value = strings.TrimSpace(value); value != "" {
			args = append(args, value)
			clauses = append(clauses, fmt.Sprintf("%s=$%d", column, len(args)))
		}
	}
	addExact("endpoint", query.Endpoint)
	addExact("model", query.Model)
	addExact("local_evaluation_state", query.EvaluationState)
	addExact("local_outcome", query.Outcome)
	if query.APIKeyID > 0 {
		args = append(args, query.APIKeyID)
		clauses = append(clauses, fmt.Sprintf("api_key_id=$%d", len(args)))
	}
	if query.LocalMiss != nil {
		if *query.LocalMiss {
			clauses = append(clauses, "upstream_error_code='cyber_policy' AND local_evaluation_state='completed' AND local_outcome='no_hit'")
		} else {
			clauses = append(clauses, "NOT (upstream_error_code='cyber_policy' AND local_evaluation_state='completed' AND local_outcome='no_hit')")
		}
	}
	if q := strings.TrimSpace(query.Query); q != "" {
		args = append(args, "%"+strings.ToLower(q)+"%")
		i := len(args)
		clauses = append(clauses, fmt.Sprintf(`(LOWER(COALESCE(prompt_preview,'')) LIKE $%d OR LOWER(COALESCE(prompt_text,'')) LIKE $%d OR LOWER(COALESCE(local_matched_patterns,'')) LIKE $%d OR LOWER(COALESCE(upstream_error,'')) LIKE $%d OR LOWER(COALESCE(api_key_name,'')) LIKE $%d)`, i, i, i, i, i))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

type promptPolicyIncidentScanner interface{ Scan(dest ...any) error }

func scanPromptPolicyIncident(scanner promptPolicyIncidentScanner) (*PromptPolicyIncident, error) {
	item := &PromptPolicyIncident{}
	var createdAtRaw any
	var score, rawScore, auditScore, auditRawScore sql.NullInt64
	if err := scanner.Scan(&item.ID, &item.IncidentID, &item.RequestCorrelationID, &createdAtRaw, &item.AttemptIndex,
		&item.Transport, &item.Endpoint, &item.Protocol, &item.Provider, &item.Model, &item.StatusCode, &item.AccountID,
		&item.APIKeyID, &item.APIKeyName, &item.APIKeyMasked, &item.Platform, &item.SourceRef, &item.UpstreamErrorCode,
		&item.UpstreamError, &item.LocalEvaluationState, &item.LocalOutcome, &item.LocalAction, &score, &rawScore, &auditScore,
		&auditRawScore, &item.LocalThreshold, &item.LocalMode, &item.LocalPolicyProfile, &item.LocalReasonCode,
		&item.LocalReason, &item.LocalPrimaryOrigin, &item.LocalStrikeEligible, &item.LocalReviewModel, &item.LocalReviewFlagged,
		&item.LocalReviewError, &item.LocalMatchedPatterns, &item.PromptFingerprint, &item.PromptPreview, &item.PromptText,
		&item.CandidateID, &item.CandidateEvidenceID); err != nil {
		return nil, err
	}
	createdAt, err := parseDBTimeValue(createdAtRaw)
	if err != nil {
		return nil, err
	}
	item.CreatedAt = createdAt
	if score.Valid {
		v := int(score.Int64)
		item.LocalScore = &v
	}
	if rawScore.Valid {
		v := int(rawScore.Int64)
		item.LocalRawScore = &v
	}
	if auditScore.Valid {
		v := int(auditScore.Int64)
		item.LocalAuditScore = &v
	}
	if auditRawScore.Valid {
		v := int(auditRawScore.Int64)
		item.LocalAuditRawScore = &v
	}
	item.LocalMiss = item.UpstreamErrorCode == "cyber_policy" && item.LocalEvaluationState == PromptPolicyEvaluationCompleted && item.LocalOutcome == PromptPolicyOutcomeNoHit
	return item, nil
}

func (db *DB) ClearPromptPolicyIncidents(ctx context.Context) error {
	if db == nil {
		return nil
	}
	if db.isSQLite() {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_policy_incidents`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name='prompt_policy_incidents'`)
		return err
	}
	_, err := db.conn.ExecContext(ctx, `TRUNCATE TABLE prompt_policy_incidents RESTART IDENTITY`)
	return err
}
