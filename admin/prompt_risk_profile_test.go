package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRiskProfileListAndDetailAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-risk-profile.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	zero := 0
	incident := database.PromptPolicyIncidentInput{
		IncidentID: "risk-profile-incident", RequestCorrelationID: "risk-profile-request",
		Endpoint: "/v1/responses", Protocol: "responses", Transport: "sse", Model: "gpt-5.6-sol",
		StatusCode: 500, AccountID: 73, AccountName: "account@example.com", APIKeyID: 9, APIKeyName: "tenant-key",
		UpstreamErrorCode: "cyber_policy", LocalEvaluationState: database.PromptPolicyEvaluationCompleted,
		LocalOutcome: database.PromptPolicyOutcomeNoHit, LocalComparison: database.PromptPolicyComparisonConfirmedMiss,
		LocalScore: &zero, LocalRawScore: &zero, LocalAuditScore: &zero, LocalAuditRawScore: &zero,
		PromptFingerprint: adminPolicyFingerprint("risk-profile-prompt"), PromptPreview: "risk-profile-prompt", PromptText: "risk-profile-prompt", PromptAvailable: true,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "newapi", NewAPIUserID: "user-42", SessionHash: "session-hash", ClientIPHash: "ip-hash",
		ObservedAt: time.Now().UTC(),
	}
	candidate := database.PromptRuleCandidateInput{Fingerprint: incident.PromptFingerprint, Kind: database.PromptRuleCandidateKindEvidence, Source: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: incident.PromptPreview}
	evidence := database.PromptRuleCandidateEvidenceInput{SourceKind: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: incident.RequestCorrelationID, SourceRefHash: adminPolicyFingerprint(incident.IncidentID), MetadataJSON: `{}`, ObservedAt: incident.ObservedAt}
	if err := db.PersistPromptPolicyIncident(t.Context(), incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}

	h := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/risk-profiles", h.ListPromptRiskProfiles)
	router.GET("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)

	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/risk-profiles?subject_type=newapi_user&platform=newapi&page=1&page_size=1", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Profiles       []database.PromptRiskProfile `json:"profiles"`
		Total          int                          `json:"total"`
		Page           int                          `json:"page"`
		PageSize       int                          `json:"page_size"`
		ScoringVersion string                       `json:"scoring_version"`
		Guardrail      string                       `json:"guardrail"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || list.Total != 1 || len(list.Profiles) != 1 || !list.Profiles[0].IsPerson || list.Profiles[0].SubjectDisplay != "user-42" || list.ScoringVersion != database.PromptRiskScoringVersion || list.Guardrail == "" {
		t.Fatalf("list response=%s err=%v", listRecorder.Body.String(), err)
	}

	detailRecorder := httptest.NewRecorder()
	path := "/api/admin/prompt-policy/risk-profiles/newapi_user/" + list.Profiles[0].SubjectKey + "?event_page=1&event_page_size=1"
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, path, nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		Profile       database.PromptRiskProfile `json:"profile"`
		Events        []database.PromptRiskEvent `json:"events"`
		EventTotal    int                        `json:"event_total"`
		EventPage     int                        `json:"event_page"`
		EventPageSize int                        `json:"event_page_size"`
		Guardrail     string                     `json:"guardrail"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil || detail.EventTotal != 1 || len(detail.Events) != 1 || detail.Events[0].IncidentID != incident.IncidentID || detail.Profile.RiskScore <= 0 || detail.Guardrail == "" {
		t.Fatalf("detail response=%s err=%v", detailRecorder.Body.String(), err)
	}
}

func TestPromptRiskProfileDetailRejectsUnknownSubjectType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/risk-profiles/person/raw-user-id", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
