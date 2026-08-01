package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const promptRiskHistoryGuardrail = "画像只统计本地 warn/block 与上游 CY；影子审计和普通命中不再抬高风险。画像不会单独封禁当前请求，只控制可自动失效的模型复核豁免；达到阈值或再次出现 CY 时立即恢复同步审核。"

type promptRiskProfilesResponse struct {
	Profiles       []*database.PromptRiskProfile `json:"profiles"`
	Total          int                           `json:"total"`
	Page           int                           `json:"page"`
	PageSize       int                           `json:"page_size"`
	ScoringVersion string                        `json:"scoring_version"`
	Guardrail      string                        `json:"guardrail"`
}

type promptRiskProfileDetailResponse struct {
	Profile        *database.PromptRiskProfile      `json:"profile"`
	Events         []*database.PromptRiskEvent      `json:"events"`
	TrustEvents    []*database.PromptRiskTrustEvent `json:"trust_events"`
	EventTotal     int                              `json:"event_total"`
	EventPage      int                              `json:"event_page"`
	EventPageSize  int                              `json:"event_page_size"`
	ScoringVersion string                           `json:"scoring_version"`
	Guardrail      string                           `json:"guardrail"`
}

func (h *Handler) ListPromptRiskProfiles(c *gin.Context) {
	page := positiveQueryInt(c, "page", 1)
	pageSize := positiveQueryInt(c, "page_size", 20)
	apiKeyID := promptRiskPositiveInt64(c.Query("api_key_id"))
	accountID := promptRiskPositiveInt64(c.Query("account_id"))
	minScore := positiveQueryInt(c, "min_score", 0)
	if minScore > 100 {
		minScore = 100
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	profiles, total, err := h.db.ListPromptRiskProfiles(ctx, database.PromptRiskProfileQuery{
		Page: page, PageSize: pageSize, SubjectType: c.Query("subject_type"), Platform: c.Query("platform"),
		RiskLevel: c.Query("risk_level"), APIKeyID: apiKeyID, AccountID: accountID, MinScore: minScore, Query: c.Query("q"),
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if profiles == nil {
		profiles = []*database.PromptRiskProfile{}
	}
	h.attachPromptRiskTrustPolicies(ctx, profiles)
	c.JSON(http.StatusOK, promptRiskProfilesResponse{
		Profiles: profiles, Total: total, Page: page, PageSize: pageSize,
		ScoringVersion: database.PromptRiskScoringVersion, Guardrail: promptRiskHistoryGuardrail,
	})
}

func (h *Handler) GetPromptRiskProfile(c *gin.Context) {
	subjectType := strings.TrimSpace(c.Param("subject_type"))
	subjectKey := strings.TrimSpace(c.Param("subject_key"))
	if !validPromptRiskSubjectType(subjectType) || subjectKey == "" {
		writeError(c, http.StatusBadRequest, "风险画像标识无效")
		return
	}
	eventPage := positiveQueryInt(c, "event_page", 1)
	eventPageSize := positiveQueryInt(c, "event_page_size", 20)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	profile, err := h.db.GetPromptRiskProfile(ctx, subjectType, subjectKey)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "风险画像不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	events, total, err := h.db.ListPromptRiskEvents(ctx, subjectType, subjectKey, database.PromptRiskEventQuery{Page: eventPage, PageSize: eventPageSize})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if events == nil {
		events = []*database.PromptRiskEvent{}
	}
	h.attachPromptRiskTrustPolicies(ctx, []*database.PromptRiskProfile{profile})
	trustEvents, err := h.db.ListPromptRiskTrustEvents(ctx, subjectType, subjectKey, 100)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, promptRiskProfileDetailResponse{
		Profile: profile, Events: events, TrustEvents: trustEvents, EventTotal: total, EventPage: eventPage, EventPageSize: eventPageSize,
		ScoringVersion: database.PromptRiskScoringVersion, Guardrail: promptRiskHistoryGuardrail,
	})
}

func (h *Handler) attachPromptRiskTrustPolicies(ctx context.Context, profiles []*database.PromptRiskProfile) {
	if h == nil || h.db == nil || len(profiles) == 0 {
		return
	}
	policies, err := h.db.ListAllPromptRiskTrustPolicies(ctx, "all")
	if err != nil {
		return
	}
	bySubject := make(map[string]*database.PromptRiskTrustPolicy, len(policies))
	for _, policy := range policies {
		if policy != nil {
			bySubject[policy.SubjectType+"\x00"+policy.SubjectKey] = policy
		}
	}
	for _, profile := range profiles {
		if profile != nil {
			profile.TrustPolicy = bySubject[profile.SubjectType+"\x00"+profile.SubjectKey]
		}
	}
}

func promptRiskPositiveInt64(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func validPromptRiskSubjectType(value string) bool {
	switch value {
	case database.PromptRiskSubjectNewAPIUser, database.PromptRiskSubjectSession, database.PromptRiskSubjectAPIKey,
		database.PromptRiskSubjectClientIP, database.PromptRiskSubjectUpstreamAccount:
		return true
	default:
		return false
	}
}
