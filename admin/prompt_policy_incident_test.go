package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func adminPolicyFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestPromptPolicyIncidentListAndDetailAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-policy.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	zero := 0
	incident := database.PromptPolicyIncidentInput{
		IncidentID: "incident-admin", RequestCorrelationID: "request-admin", Endpoint: "/v1/responses", Model: "gpt-5.4",
		Protocol: "responses", Transport: "sse", StatusCode: 400, AccountID: 73, AccountName: "account@example.com", AccountGroupIDs: []int64{4}, AccountGroupNames: []string{"打铁"},
		APIKeyID: 9, APIKeyName: "test-key", APIKeyAllowedGroupIDs: []int64{1, 4}, APIKeyAllowedGroupNames: []string{"凡人", "打铁"}, UpstreamErrorCode: "cyber_policy",
		LocalEvaluationState: database.PromptPolicyEvaluationCompleted, LocalOutcome: database.PromptPolicyOutcomeNoHit,
		LocalScore: &zero, LocalRawScore: &zero, LocalAuditScore: &zero, LocalAuditRawScore: &zero,
		LocalMatchedPatterns: `[{"name":"test","weight":0}]`, PromptFingerprint: adminPolicyFingerprint("prompt"), PromptPreview: "prompt", PromptText: "prompt",
		PromptAvailable: true, LocalComparison: database.PromptPolicyComparisonConfirmedMiss,
		ObservedAt: time.Now().UTC(),
	}
	candidate := database.PromptRuleCandidateInput{Fingerprint: incident.PromptFingerprint, Kind: database.PromptRuleCandidateKindEvidence, Source: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: "prompt"}
	evidence := database.PromptRuleCandidateEvidenceInput{SourceKind: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: "request-admin", SourceRefHash: adminPolicyFingerprint("incident-admin"), MetadataJSON: `{}`, ObservedAt: incident.ObservedAt}
	if err := db.PersistPromptPolicyIncident(t.Context(), incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	second := incident
	second.IncidentID = "incident-admin-second"
	second.RequestCorrelationID = "request-admin-second"
	second.PromptFingerprint = adminPolicyFingerprint("prompt-second")
	second.PromptPreview = "prompt-second"
	second.PromptText = "prompt-second"
	second.ObservedAt = second.ObservedAt.Add(time.Second)
	secondCandidate := candidate
	secondCandidate.Fingerprint = second.PromptFingerprint
	secondEvidence := evidence
	secondEvidence.SourceRef = second.RequestCorrelationID
	secondEvidence.SourceRefHash = adminPolicyFingerprint(second.IncidentID)
	secondEvidence.ObservedAt = second.ObservedAt
	if err := db.PersistPromptPolicyIncident(t.Context(), second, secondCandidate, secondEvidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident(second): %v", err)
	}

	h := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/incidents", h.ListPromptPolicyIncidents)
	router.GET("/api/admin/prompt-policy/incidents/:incident_id", h.GetPromptPolicyIncident)

	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents?local_miss=true&outcome=no_hit&account_id=73&page=2&page_size=1", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Incidents []database.PromptPolicyIncident `json:"incidents"`
		Total     int                             `json:"total"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || list.Total != 2 || len(list.Incidents) != 1 || !list.Incidents[0].LocalMiss || list.Incidents[0].AccountName != "account@example.com" || len(list.Incidents[0].AccountGroupNames) != 1 {
		t.Fatalf("list response=%s err=%v", listRecorder.Body.String(), err)
	}

	detailRecorder := httptest.NewRecorder()
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents/incident-admin", nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		Incident  database.PromptPolicyIncident `json:"incident"`
		Matches   []map[string]any              `json:"matches"`
		Candidate map[string]any                `json:"candidate"`
		Evidence  map[string]any                `json:"evidence"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil || detail.Incident.IncidentID != "incident-admin" || len(detail.Matches) != 1 || detail.Candidate == nil || detail.Evidence == nil {
		t.Fatalf("detail response=%s err=%v", detailRecorder.Body.String(), err)
	}
}
