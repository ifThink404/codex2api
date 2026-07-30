package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	promptPolicyDDLDriverOnce sync.Once
	promptPolicyDDLQueryMu    sync.Mutex
	promptPolicyDDLQueries    []string
)

type promptPolicyDDLDriver struct{}
type promptPolicyDDLConn struct{}

func (promptPolicyDDLDriver) Open(string) (driver.Conn, error) { return promptPolicyDDLConn{}, nil }
func (promptPolicyDDLConn) Prepare(string) (driver.Stmt, error) {
	return nil, nil
}
func (promptPolicyDDLConn) Close() error              { return nil }
func (promptPolicyDDLConn) Begin() (driver.Tx, error) { return nil, nil }
func (promptPolicyDDLConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	promptPolicyDDLQueryMu.Lock()
	promptPolicyDDLQueries = append(promptPolicyDDLQueries, query)
	promptPolicyDDLQueryMu.Unlock()
	return driver.RowsAffected(0), nil
}

func promptPolicyTestFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func newPromptPolicySQLiteTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "prompt-policy.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func promptPolicyTestInputs(incidentID string) (PromptPolicyIncidentInput, PromptRuleCandidateInput, PromptRuleCandidateEvidenceInput) {
	observedAt := time.Now().UTC().Truncate(time.Millisecond)
	zero := 0
	incident := PromptPolicyIncidentInput{
		IncidentID: incidentID, RequestCorrelationID: "request-1", AttemptIndex: 2, Transport: "sse",
		Endpoint: "/v1/responses", Protocol: "responses", Provider: "openai", Model: "gpt-5.4",
		StatusCode: 400, AccountID: 7, APIKeyID: 9, APIKeyName: "test", UpstreamErrorCode: "cyber_policy",
		UpstreamError: `{"error":{"code":"cyber_policy"}}`, LocalEvaluationState: PromptPolicyEvaluationCompleted,
		LocalOutcome: PromptPolicyOutcomeNoHit, LocalAction: "allow", LocalScore: &zero, LocalRawScore: &zero,
		LocalAuditScore: &zero, LocalAuditRawScore: &zero, LocalThreshold: 50, LocalMode: "block",
		LocalMatchedPatterns: "[]", PromptFingerprint: promptPolicyTestFingerprint("prompt"), PromptPreview: "prompt", PromptText: "prompt",
		ObservedAt: observedAt,
	}
	candidate := PromptRuleCandidateInput{
		Fingerprint: incident.PromptFingerprint, Kind: PromptRuleCandidateKindEvidence,
		Source: PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: incident.PromptPreview,
	}
	evidence := PromptRuleCandidateEvidenceInput{
		SourceKind: PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: "request-1",
		SourceRefHash: promptPolicyTestFingerprint(incidentID), MetadataJSON: `{}`,
		Protocol: "responses", Provider: "openai", Model: "gpt-5.4", APIKeyID: 9, APIKeyName: "test", ObservedAt: observedAt,
	}
	return incident, candidate, evidence
}

func TestPromptPolicyIncidentPersistsNullableScoresAndExactEvidenceLink(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incident, candidate, evidence := promptPolicyTestInputs("incident-zero")
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	got, err := db.GetPromptPolicyIncident(ctx, incident.IncidentID)
	if err != nil {
		t.Fatalf("GetPromptPolicyIncident: %v", err)
	}
	if got.LocalScore == nil || *got.LocalScore != 0 || got.LocalAuditScore == nil || *got.LocalAuditScore != 0 {
		t.Fatalf("real zero scores were not preserved: %#v", got)
	}
	if !got.LocalMiss || got.CandidateID == 0 || got.CandidateEvidenceID == 0 {
		t.Fatalf("incident linkage/local_miss = %#v", got)
	}
	items, err := db.ListPromptRuleCandidateEvidence(ctx, got.CandidateID, 10)
	if err != nil || len(items) != 1 || items[0].ID != got.CandidateEvidenceID || items[0].PromptPolicyIncidentID != incident.IncidentID {
		t.Fatalf("candidate evidence link items=%#v err=%v", items, err)
	}

	notRun, notRunCandidate, notRunEvidence := promptPolicyTestInputs("incident-not-run")
	notRun.LocalEvaluationState = PromptPolicyEvaluationNotRun
	notRun.LocalOutcome = PromptPolicyOutcomeNoHit
	notRun.LocalScore, notRun.LocalRawScore, notRun.LocalAuditScore, notRun.LocalAuditRawScore = nil, nil, nil, nil
	notRun.PromptFingerprint = promptPolicyTestFingerprint("not-run")
	notRunCandidate.Fingerprint = notRun.PromptFingerprint
	notRunEvidence.SourceRefHash = promptPolicyTestFingerprint(notRun.IncidentID)
	if err := db.PersistPromptPolicyIncident(ctx, notRun, notRunCandidate, notRunEvidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident(not_run): %v", err)
	}
	got, err = db.GetPromptPolicyIncident(ctx, notRun.IncidentID)
	if err != nil || got.LocalScore != nil || got.LocalAuditScore != nil || got.LocalMiss {
		t.Fatalf("not_run nullable/local_miss got=%#v err=%v", got, err)
	}
}

func TestPromptPolicyIncidentCompositeTransactionRollsBack(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER fail_policy_evidence BEFORE INSERT ON prompt_rule_candidate_evidence BEGIN SELECT RAISE(ABORT, 'forced evidence failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	incident, candidate, evidence := promptPolicyTestInputs("incident-rollback")
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err == nil {
		t.Fatal("PersistPromptPolicyIncident unexpectedly succeeded")
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents WHERE incident_id=$1`, incident.IncidentID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("incident transaction was not rolled back count=%d err=%v", count, err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidates WHERE fingerprint=$1`, candidate.Fingerprint).Scan(&count); err != nil || count != 0 {
		t.Fatalf("candidate transaction was not rolled back count=%d err=%v", count, err)
	}
}

func TestClearPromptFilterLogsClearsIncidentsButKeepsCandidateEvidence(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incident, candidate, evidence := promptPolicyTestInputs("incident-clear")
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	if err := db.ClearPromptFilterLogs(ctx); err != nil {
		t.Fatalf("ClearPromptFilterLogs: %v", err)
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("incidents not cleared count=%d err=%v", count, err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidates`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("candidate unexpectedly cleared count=%d err=%v", count, err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidate_evidence`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("evidence unexpectedly cleared count=%d err=%v", count, err)
	}
}

func TestLegacyPromptFilterLogMigratesWithoutInventingScores(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "upstream_cyber_policy", Endpoint: "/v1/responses", Model: "gpt-5.4", ErrorCode: "cyber_policy", FullText: "legacy redacted error",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	if err := db.migrateLegacyPromptPolicyIncidents(ctx); err != nil {
		t.Fatalf("migrateLegacyPromptPolicyIncidents: %v", err)
	}
	items, total, err := db.ListPromptPolicyIncidentsPage(ctx, PromptPolicyIncidentQuery{Page: 1, PageSize: 10})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("legacy incidents total=%d items=%#v err=%v", total, items, err)
	}
	if items[0].LocalEvaluationState != PromptPolicyEvaluationLegacyUnknown || items[0].LocalScore != nil || items[0].PromptText != "" || items[0].RequestCorrelationID != "" {
		t.Fatalf("legacy incident invented local data: %#v", items[0])
	}
}

func TestPromptPolicyIncidentSQLiteSchemaAndIndexes(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for table, expected := range map[string][]string{
		"usage_logs":                     {"prompt_policy_incident_id"},
		"prompt_rule_candidate_evidence": {"prompt_policy_incident_id"},
		"prompt_filter_logs":             {"request_correlation_id"},
		"prompt_policy_incidents":        {"incident_id", "request_correlation_id", "local_score", "local_audit_score", "candidate_id", "candidate_evidence_id"},
	} {
		columns, err := db.sqliteTableColumns(ctx, table)
		if err != nil {
			t.Fatalf("sqliteTableColumns(%s): %v", table, err)
		}
		for _, name := range expected {
			if _, ok := columns[name]; !ok {
				t.Fatalf("%s missing column %q", table, name)
			}
		}
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='prompt_policy_incidents'`)
	if err != nil {
		t.Fatalf("list incident indexes: %v", err)
	}
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan incident index: %v", err)
		}
		indexes[name] = true
	}
	for _, name := range []string{
		"idx_prompt_policy_incidents_request", "idx_prompt_policy_incidents_created", "idx_prompt_policy_incidents_api_key",
		"idx_prompt_policy_incidents_endpoint", "idx_prompt_policy_incidents_outcome",
	} {
		if !indexes[name] {
			t.Fatalf("prompt_policy_incidents missing index %q", name)
		}
	}
}

func TestPromptPolicyIncidentPostgresMigrationDDL(t *testing.T) {
	promptPolicyDDLDriverOnce.Do(func() { sql.Register("prompt-policy-ddl-capture", promptPolicyDDLDriver{}) })
	promptPolicyDDLQueryMu.Lock()
	promptPolicyDDLQueries = nil
	promptPolicyDDLQueryMu.Unlock()
	conn, err := sql.Open("prompt-policy-ddl-capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn, driver: "postgres"}
	if err := db.ensurePromptPolicyIncidentsTable(context.Background()); err != nil {
		t.Fatalf("ensurePromptPolicyIncidentsTable: %v", err)
	}
	promptPolicyDDLQueryMu.Lock()
	joined := strings.Join(promptPolicyDDLQueries, "\n")
	promptPolicyDDLQueryMu.Unlock()
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS prompt_policy_incidents",
		"incident_id VARCHAR(64) NOT NULL UNIQUE",
		"local_score INT NULL",
		"local_audit_score INT NULL",
		"ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS prompt_policy_incident_id",
		"ALTER TABLE prompt_rule_candidate_evidence ADD COLUMN IF NOT EXISTS prompt_policy_incident_id",
		"ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS request_correlation_id",
		"idx_prompt_policy_incidents_request",
		"idx_prompt_policy_incidents_outcome",
		"legacy-",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("postgres incident migration missing %q: %s", fragment, joined)
		}
	}
}

func TestUsageLogIncidentIDSurvivesEveryDetailQueryPath(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incidentID := "incident-usage-query-paths"
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID: 1, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 400,
		AttemptIndex: 2, UpstreamErrorKind: "cyber_policy", PromptPolicyIncidentID: incidentID,
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	db.FlushUsageLogs()
	assertID := func(name string, logs []*UsageLog, err error) {
		t.Helper()
		if err != nil || len(logs) != 1 || logs[0].PromptPolicyIncidentID != incidentID {
			t.Fatalf("%s logs=%#v err=%v", name, logs, err)
		}
	}
	recent, err := db.ListRecentUsageLogs(ctx, 10)
	assertID("recent", recent, err)
	ranged, err := db.ListUsageLogsByTimeRange(ctx, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	assertID("time_range", ranged, err)
	paged, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), Page: 1, PageSize: 10, IncludeCanceled: true,
	})
	if err != nil || paged == nil {
		t.Fatalf("paged query: %#v err=%v", paged, err)
	}
	assertID("paged", paged.Logs, nil)
}
