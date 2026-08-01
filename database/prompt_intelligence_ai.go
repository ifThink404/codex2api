package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const (
	PromptRuleCandidateSourceAIAnalysis         = "ai_analysis"
	PromptRuleCandidateSourceAIIdentityUpdate   = "ai_identity_update"
	PromptRuleCandidateSourceAIIdentityRollback = "ai_identity_rollback"
)

// AddPromptRuleCandidateEvidence attaches one deduplicated audit event to an
// existing candidate without changing its kind, source, or proposed rule.
func (db *DB) AddPromptRuleCandidateEvidence(ctx context.Context, candidateID int64, raw PromptRuleCandidateEvidenceInput) (*PromptRuleCandidateEvidence, bool, error) {
	if db == nil {
		return nil, false, errors.New("database is nil")
	}
	if candidateID <= 0 {
		return nil, false, errors.New("candidate ID is invalid")
	}
	evidence, err := normalizePromptRuleCandidateEvidenceInput(raw)
	if err != nil {
		return nil, false, err
	}
	var evidenceID int64
	var added bool
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		var exists int
		if scanErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidates WHERE id=$1`, candidateID).Scan(&exists); scanErr != nil {
			return scanErr
		}
		if exists == 0 {
			return sql.ErrNoRows
		}
		result, execErr := tx.ExecContext(ctx, `
			INSERT INTO prompt_rule_candidate_evidence (
				candidate_id, source_kind, source_ref, source_ref_hash, sample_preview, metadata_json,
				request_protocol, request_provider, model, api_key_id, api_key_name, prompt_policy_incident_id,
				observed_at, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,CURRENT_TIMESTAMP)
			ON CONFLICT(candidate_id, source_kind, source_ref_hash) DO NOTHING
		`, candidateID, evidence.SourceKind, evidence.SourceRef, evidence.SourceRefHash, evidence.SamplePreview,
			evidence.MetadataJSON, evidence.Protocol, evidence.Provider, evidence.Model, evidence.APIKeyID,
			evidence.APIKeyName, evidence.PromptPolicyIncidentID, evidence.ObservedAt)
		if execErr != nil {
			return execErr
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return affectedErr
		}
		added = affected > 0
		if added {
			result, execErr = tx.ExecContext(ctx, `
				UPDATE prompt_rule_candidates SET evidence_count=evidence_count+1,
					updated_at=CURRENT_TIMESTAMP,
					last_seen_at=CASE WHEN $1 > last_seen_at THEN $1 ELSE last_seen_at END
				WHERE id=$2
			`, evidence.ObservedAt, candidateID)
			if execErr != nil {
				return execErr
			}
			if affectedErr := requireAffectedRow(result); affectedErr != nil {
				return affectedErr
			}
		}
		if scanErr := tx.QueryRowContext(ctx, `
			SELECT id FROM prompt_rule_candidate_evidence
			WHERE candidate_id=$1 AND source_kind=$2 AND source_ref_hash=$3
		`, candidateID, evidence.SourceKind, evidence.SourceRefHash).Scan(&evidenceID); scanErr != nil {
			return scanErr
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, false, err
	}
	item, err := db.GetPromptRuleCandidateEvidence(ctx, evidenceID)
	return item, added, err
}

func (db *DB) GetPromptRuleCandidateEvidence(ctx context.Context, evidenceID int64) (*PromptRuleCandidateEvidence, error) {
	item := &PromptRuleCandidateEvidence{}
	var observedRaw, createdRaw any
	err := db.conn.QueryRowContext(ctx, `
		SELECT id, candidate_id, source_kind, source_ref, source_ref_hash, sample_preview, metadata_json,
		       request_protocol, request_provider, model, api_key_id, api_key_name,
		       COALESCE(prompt_policy_incident_id, ''), observed_at, created_at
		FROM prompt_rule_candidate_evidence WHERE id=$1
	`, evidenceID).Scan(&item.ID, &item.CandidateID, &item.SourceKind, &item.SourceRef, &item.SourceRefHash,
		&item.SamplePreview, &item.MetadataJSON, &item.Protocol, &item.Provider, &item.Model,
		&item.APIKeyID, &item.APIKeyName, &item.PromptPolicyIncidentID, &observedRaw, &createdRaw)
	if err != nil {
		return nil, err
	}
	if item.ObservedAt, err = parseDBTimeValue(observedRaw); err != nil {
		return nil, err
	}
	if item.CreatedAt, err = parseDBTimeValue(createdRaw); err != nil {
		return nil, err
	}
	return item, nil
}

// CompareAndSwapPromptFilterAdvancedConfigWithEvidence changes only the
// persisted advanced Prompt configuration and commits its identity audit event
// in the same transaction. This prevents an applied identity change from
// existing without a durable revision record.
func (db *DB) CompareAndSwapPromptFilterAdvancedConfigWithEvidence(
	ctx context.Context,
	candidateID int64,
	expectedJSON, replacementJSON string,
	rawEvidence PromptRuleCandidateEvidenceInput,
) (bool, *PromptRuleCandidateEvidence, error) {
	expectedJSON = strings.TrimSpace(expectedJSON)
	replacementJSON = strings.TrimSpace(replacementJSON)
	if expectedJSON == "" {
		expectedJSON = "{}"
	}
	if replacementJSON == "" || !json.Valid([]byte(expectedJSON)) || !json.Valid([]byte(replacementJSON)) {
		return false, nil, errors.New("advanced config compare-and-swap JSON is invalid")
	}
	if candidateID <= 0 {
		return false, nil, errors.New("candidate ID is invalid")
	}
	evidence, err := normalizePromptRuleCandidateEvidenceInput(rawEvidence)
	if err != nil {
		return false, nil, err
	}
	var swapped bool
	var evidenceID int64
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		settingsQuery := `SELECT COALESCE(NULLIF(TRIM(prompt_filter_advanced_config), ''), '{}') FROM system_settings WHERE id=1`
		if !db.isSQLite() {
			settingsQuery += ` FOR UPDATE`
		}
		var currentJSON string
		if scanErr := tx.QueryRowContext(ctx, settingsQuery).Scan(&currentJSON); scanErr != nil {
			return scanErr
		}
		if strings.TrimSpace(currentJSON) != expectedJSON {
			return nil
		}
		result, execErr := tx.ExecContext(ctx, `
			UPDATE system_settings SET prompt_filter_advanced_config=$1
			WHERE id=1 AND COALESCE(NULLIF(TRIM(prompt_filter_advanced_config), ''), '{}')=$2
		`, replacementJSON, expectedJSON)
		if execErr != nil {
			return execErr
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return affectedErr
		}
		if affected == 0 {
			return nil
		}
		result, execErr = tx.ExecContext(ctx, `
			INSERT INTO prompt_rule_candidate_evidence (
				candidate_id, source_kind, source_ref, source_ref_hash, sample_preview, metadata_json,
				request_protocol, request_provider, model, api_key_id, api_key_name, prompt_policy_incident_id,
				observed_at, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,CURRENT_TIMESTAMP)
			ON CONFLICT(candidate_id, source_kind, source_ref_hash) DO NOTHING
		`, candidateID, evidence.SourceKind, evidence.SourceRef, evidence.SourceRefHash, evidence.SamplePreview,
			evidence.MetadataJSON, evidence.Protocol, evidence.Provider, evidence.Model, evidence.APIKeyID,
			evidence.APIKeyName, evidence.PromptPolicyIncidentID, evidence.ObservedAt)
		if execErr != nil {
			return execErr
		}
		evidenceAffected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return affectedErr
		}
		if evidenceAffected == 0 {
			return errors.New("identity revision evidence already exists")
		}
		if scanErr := tx.QueryRowContext(ctx, `
			SELECT id FROM prompt_rule_candidate_evidence
			WHERE candidate_id=$1 AND source_kind=$2 AND source_ref_hash=$3
		`, candidateID, evidence.SourceKind, evidence.SourceRefHash).Scan(&evidenceID); scanErr != nil {
			return scanErr
		}
		result, execErr = tx.ExecContext(ctx, `
			UPDATE prompt_rule_candidates SET evidence_count=evidence_count+1,
				updated_at=CURRENT_TIMESTAMP,
				last_seen_at=CASE WHEN $1 > last_seen_at THEN $1 ELSE last_seen_at END
			WHERE id=$2
		`, evidence.ObservedAt, candidateID)
		if execErr != nil {
			return execErr
		}
		if affectedErr := requireAffectedRow(result); affectedErr != nil {
			return affectedErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		swapped = true
		return nil
	})
	if err != nil || !swapped {
		return swapped, nil, err
	}
	item, err := db.GetPromptRuleCandidateEvidence(ctx, evidenceID)
	return true, item, err
}
