package database

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func findPromptRiskProfile(items []*PromptRiskProfile, subjectType string) *PromptRiskProfile {
	for _, item := range items {
		if item.SubjectType == subjectType {
			return item
		}
	}
	return nil
}

func TestPromptRiskProfilesSeparatePersonTenantNetworkAndAccount(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	logInput := &PromptFilterLogInput{
		Source: "local_filter", Endpoint: "/v1/responses", Model: "gpt-5.6-sol", Action: "block", Score: 95,
		AuditScore: 95, ReasonCode: "terminal_category", StrikeEligible: true, MatchedPatterns: `[{"name":"terminal","weight":95}]`,
		TextPreview: "redacted prompt", APIKeyID: 9, APIKeyName: "tenant-key", APIKeyMasked: "sk-***9",
		NewAPIPolicyStatus: "signed_response", NewAPIPlatform: "newapi-prod", NewAPIUserID: "user-42",
		NewAPIRequestID: "req-42", NewAPIDecisionID: "dec-42", SessionHash: promptRiskHash("session", "session-42"),
		ClientIPHash: promptRiskHash("client-ip", "203.0.113.8"), RequestCorrelationID: "corr-42",
	}
	if err := db.InsertPromptFilterLog(ctx, logInput); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	incident, candidate, evidence := promptPolicyTestInputs("risk-incident-42")
	incident.LocalComparison = PromptPolicyComparisonConfirmedMiss
	incident.NewAPIPolicyStatus = "verified"
	incident.NewAPIPlatform = "newapi-prod"
	incident.NewAPIUserID = "user-42"
	incident.NewAPIUserName = "示例平台用户"
	incident.NewAPIUserEmail = "gateway-a-user@example.com"
	incident.NewAPIUserGroup = "gateway-a-paid"
	incident.NewAPIRequestID = "req-cy-42"
	incident.SessionHash = logInput.SessionHash
	incident.ClientIPHash = logInput.ClientIPHash
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles: %v", err)
	}
	if total != 5 {
		t.Fatalf("profile total = %d, want 5: %#v", total, profiles)
	}
	user := findPromptRiskProfile(profiles, PromptRiskSubjectNewAPIUser)
	if user == nil || !user.IsPerson || user.IdentityConfidence != 100 || user.EventCount != 2 || user.ConfirmedMissCount != 1 || user.RiskScore < 60 {
		t.Fatalf("signed user profile = %#v", user)
	}
	if user.SubjectDisplay != "示例平台用户" || user.NewAPIUserID != "user-42" || user.NewAPIUserEmail != "gateway-a-user@example.com" || user.NewAPIUserGroup != "gateway-a-paid" {
		t.Fatalf("signed user identity = %#v", user)
	}
	key := findPromptRiskProfile(profiles, PromptRiskSubjectAPIKey)
	if key == nil || key.IsPerson || key.IdentityConfidence != 60 || key.APIKeyID != 9 {
		t.Fatalf("API key profile = %#v", key)
	}
	account := findPromptRiskProfile(profiles, PromptRiskSubjectUpstreamAccount)
	if account == nil || account.IsPerson || account.IdentityConfidence != 25 || account.AccountID != 7 {
		t.Fatalf("account profile = %#v", account)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectClientIP) == nil || findPromptRiskProfile(profiles, PromptRiskSubjectSession) == nil {
		t.Fatalf("network/session profiles missing: %#v", profiles)
	}
	events, eventTotal, err := db.ListPromptRiskEvents(ctx, user.SubjectType, user.SubjectKey, PromptRiskEventQuery{Page: 1, PageSize: 1})
	if err != nil || eventTotal != 2 || len(events) != 1 {
		t.Fatalf("user events total=%d items=%#v err=%v", eventTotal, events, err)
	}
	if events[0].NewAPIUserName != "示例平台用户" || events[0].NewAPIUserID != "user-42" {
		t.Fatalf("event identity = %#v", events[0])
	}
	filtered, filteredTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, Query: "gateway-a-user@example.com"})
	if err != nil || filteredTotal != 1 || len(filtered) != 1 || filtered[0].SubjectKey != user.SubjectKey {
		t.Fatalf("identity search total=%d items=%#v err=%v", filteredTotal, filtered, err)
	}
}

func TestPromptRiskIdentityDirectoryEnrichesHistoricalProfiles(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "local_filter", Action: "block", StrikeEligible: true, MatchedPatterns: `[{"name":"x"}]`,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "gateway-a", NewAPIUserID: "73",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	if err := db.UpsertPromptRiskIdentities(ctx, []PromptRiskIdentityInput{{
		Platform: "gateway-a", ExternalUserID: "73", UserName: "known-user", UserEmail: "known@example.com", UserGroup: "vip", Source: "admin_import",
	}}); err != nil {
		t.Fatalf("UpsertPromptRiskIdentities: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser, Query: "vip"})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	profile := profiles[0]
	if profile.SubjectDisplay != "known-user" || profile.NewAPIUserID != "73" || profile.NewAPIUserEmail != "known@example.com" || profile.NewAPIUserGroup != "vip" {
		t.Fatalf("historical profile identity = %#v", profile)
	}
}

func TestPromptRiskUnsignedIdentityNeverCreatesPersonProfile(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	input := &PromptFilterLogInput{
		Source: "local_filter", Action: "warn", Score: 70, MatchedPatterns: `[{"name":"x"}]`,
		APIKeyID: 10, APIKeyName: "shared", ClientIPHash: promptRiskHash("client-ip", "198.51.100.1"),
		NewAPIPolicyStatus: "verification_failed", NewAPIPlatform: "newapi-prod", NewAPIUserID: "spoofed-user",
	}
	if err := db.InsertPromptFilterLog(context.Background(), input); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	profiles, _, err := db.ListPromptRiskProfiles(context.Background(), PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles: %v", err)
	}
	if person := findPromptRiskProfile(profiles, PromptRiskSubjectNewAPIUser); person != nil {
		t.Fatalf("unverified user became person profile: %#v", person)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectAPIKey) == nil || findPromptRiskProfile(profiles, PromptRiskSubjectClientIP) == nil {
		t.Fatalf("lower-confidence scopes missing: %#v", profiles)
	}
}

func TestPromptRiskBackfillLearnsExistingCYWithoutInventingPerson(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	createdAt := time.Now().UTC().Add(-time.Hour)
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_policy_incidents (
		incident_id, created_at, api_key_id, api_key_name, account_id, account_name, upstream_error_code,
		local_evaluation_state, local_outcome, local_comparison, prompt_fingerprint, prompt_preview, prompt_available
	) VALUES ($1,$2,$3,$4,$5,$6,'cyber_policy','completed','no_hit','confirmed_miss',$7,'redacted',true)`,
		"legacy-risk-cy", createdAt, 77, "legacy-key", 88, "legacy-account", promptPolicyTestFingerprint("legacy-risk")); err != nil {
		t.Fatalf("insert legacy incident: %v", err)
	}
	if err := db.backfillPromptRiskEvents(ctx); err != nil {
		t.Fatalf("backfillPromptRiskEvents: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil || total != 2 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectNewAPIUser) != nil {
		t.Fatalf("legacy CY invented a person: %#v", profiles)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectAPIKey) == nil || findPromptRiskProfile(profiles, PromptRiskSubjectUpstreamAccount) == nil {
		t.Fatalf("legacy CY did not learn key/account scopes: %#v", profiles)
	}
	if err := db.backfillPromptRiskEvents(ctx); err != nil {
		t.Fatalf("idempotent backfill: %v", err)
	}
	_, totalAfter, _ := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if totalAfter != total {
		t.Fatalf("backfill duplicated profiles: before=%d after=%d", total, totalAfter)
	}
}

func TestPromptRiskProfilesPaginateFilterAndClearWithAuditHistory(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for _, userID := range []string{"user-a", "user-b", "user-c"} {
		if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
			Source: "local_filter", Action: "block", StrikeEligible: true, MatchedPatterns: `[{"name":"x"}]`,
			NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: userID,
		}); err != nil {
			t.Fatalf("InsertPromptFilterLog(%s): %v", userID, err)
		}
	}
	page, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 2, PageSize: 1, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 3 || len(page) != 1 {
		t.Fatalf("pagination total=%d page=%#v err=%v", total, page, err)
	}
	filtered, filteredTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser, Query: "user-b"})
	if err != nil || filteredTotal != 1 || len(filtered) != 1 || filtered[0].SubjectDisplay != "user-b" {
		t.Fatalf("filter total=%d items=%#v err=%v", filteredTotal, filtered, err)
	}
	if err := db.ClearPromptFilterLogs(ctx); err != nil {
		t.Fatalf("ClearPromptFilterLogs: %v", err)
	}
	_, total, err = db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil || total != 0 {
		t.Fatalf("profiles survived clear total=%d err=%v", total, err)
	}
}

func TestPromptRiskProfilesUseStableTieBreakerAcrossPages(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 6; index++ {
		userID := fmt.Sprintf("stable-user-%d", index)
		subjectKey := PromptRiskNewAPIUserSubjectKey("gateway-a", userID)
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, subject_type, subject_key, subject_display, platform,
			is_person, identity_confidence, event_kind, request_risk_score, evidence_confidence, action
		) VALUES ($1,'prompt_filter',$2,'newapi_user',$3,$4,'gateway-a',true,100,'local_warn',45,100,'warn')`,
			createdAt, fmt.Sprintf("stable-source-%d", index), subjectKey, userID); err != nil {
			t.Fatalf("insert stable profile: %v", err)
		}
	}
	got := make([]string, 0, 6)
	for page := 1; page <= 3; page++ {
		items, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: page, PageSize: 2, SubjectType: PromptRiskSubjectNewAPIUser})
		if err != nil || total != 6 || len(items) != 2 {
			t.Fatalf("page=%d total=%d items=%#v err=%v", page, total, items, err)
		}
		for _, item := range items {
			got = append(got, item.SubjectKey)
		}
	}
	want := append([]string(nil), got...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unstable page order got=%#v want=%#v", got, want)
	}
}

func TestPromptRiskScoringBandsAndHistoryGuardrail(t *testing.T) {
	for _, test := range []struct {
		score int
		want  string
	}{{0, PromptRiskLevelLow}, {14, PromptRiskLevelLow}, {15, PromptRiskLevelObserved}, {35, PromptRiskLevelElevated}, {60, PromptRiskLevelHigh}, {80, PromptRiskLevelCritical}} {
		if got := promptRiskLevel(test.score); got != test.want {
			t.Fatalf("promptRiskLevel(%d)=%q want %q", test.score, got, test.want)
		}
	}
	profile := PromptRiskProfile{RiskLevel: PromptRiskLevelCritical, SubjectType: PromptRiskSubjectNewAPIUser}
	actions := promptRiskRecommendations(profile)
	for _, action := range actions {
		if action == "block" || action == "ban" {
			t.Fatalf("history recommendation contains automatic enforcement: %#v", actions)
		}
	}
}

func TestPromptRiskClearedReviewDoesNotIncreaseRiskOrRecurrence(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for index := 0; index < 3; index++ {
		if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
			Source: "local_filter", Action: "allow", ReviewModel: "review-model", ReviewFlagged: false,
			NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "cleared-user",
		}); err != nil {
			t.Fatalf("InsertPromptFilterLog(%d): %v", index, err)
		}
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	if profiles[0].RiskScore != 0 || profiles[0].ScoreBreakdown.Recurrence != 0 || profiles[0].RiskLevel != PromptRiskLevelLow {
		t.Fatalf("cleared reviews increased risk: %#v", profiles[0])
	}
}

func TestPromptRiskHistoricalAuditOnlyEventsNoLongerInflateProfiles(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	subjectKey := PromptRiskNewAPIUserSubjectKey("gateway-a", "audit-only-user")
	for index := 0; index < 5; index++ {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, subject_type, subject_key, subject_display, platform,
			is_person, identity_confidence, event_kind, request_risk_score, evidence_confidence,
			action, prompt_fingerprint
		) VALUES (CURRENT_TIMESTAMP,'prompt_filter',$1,'newapi_user',$2,'audit-only-user','gateway-a',true,100,'local_audit_hit',10,100,'allow',$3)`,
			fmt.Sprintf("legacy-audit-%d", index), subjectKey, promptRiskHash("audit", fmt.Sprintf("%d", index))); err != nil {
			t.Fatalf("insert legacy audit event: %v", err)
		}
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	if profiles[0].RiskScore != 0 || profiles[0].ScoreBreakdown.Recurrence != 0 || profiles[0].RiskLevel != PromptRiskLevelLow {
		t.Fatalf("legacy audit-only events still inflate risk: %#v", profiles[0])
	}
}

func TestPromptRiskSQLiteSchemaAndIndexes(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for table, expected := range map[string][]string{
		"prompt_filter_logs":        {"session_hash"},
		"prompt_policy_incidents":   {"newapi_policy_status", "newapi_platform", "newapi_user_id", "newapi_request_id", "session_hash", "client_ip_hash"},
		"prompt_risk_events":        {"source_type", "source_id", "subject_type", "subject_key", "is_person", "identity_confidence", "request_risk_score", "evidence_confidence", "incident_id", "api_key_id", "account_id"},
		"prompt_risk_event_sources": {"source_type", "source_id", "processed_at"},
		"prompt_risk_identities":    {"subject_type", "subject_key", "platform", "external_user_id", "user_name", "user_email", "user_group", "source", "updated_at"},
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
	rows, err := db.conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='prompt_risk_events'`)
	if err != nil {
		t.Fatalf("list risk indexes: %v", err)
	}
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan risk index: %v", err)
		}
		indexes[name] = true
	}
	for _, name := range []string{"idx_prompt_risk_events_subject", "idx_prompt_risk_events_created", "idx_prompt_risk_events_kind", "idx_prompt_risk_events_incident", "idx_prompt_risk_events_api_key", "idx_prompt_risk_events_account"} {
		if !indexes[name] {
			t.Fatalf("prompt_risk_events missing index %q", name)
		}
	}
}

func TestPromptRiskPostgresMigrationDDL(t *testing.T) {
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
		"CREATE TABLE IF NOT EXISTS prompt_risk_events",
		"CREATE TABLE IF NOT EXISTS prompt_risk_event_sources",
		"UNIQUE(source_type, source_id, subject_type, subject_key)",
		"ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS newapi_policy_status",
		"ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS session_hash",
		"ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS client_ip_hash",
		"ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS session_hash",
		"idx_prompt_risk_events_subject",
		"idx_prompt_risk_events_incident",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("postgres risk migration missing %q: %s", fragment, joined)
		}
	}
}
