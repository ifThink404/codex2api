package logagent

import (
	"encoding/json"
	"errors"
	"strings"
)

// Findings 是模型给出的结构化结论。Fallback=true 表示模型输出无法解析，Summary 为
// 截断后的原始输出，其余字段为空。
type Findings struct {
	Summary    string      `json:"summary"`
	RootCauses []RootCause `json:"root_causes"`
	Actions    []Action    `json:"suggested_actions"`
	Confidence float64     `json:"confidence"`
	Fallback   bool        `json:"fallback,omitempty"`
}

type RootCause struct {
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	Category    string   `json:"category"`
	EvidenceIDs []string `json:"evidence_ids"`
	Confidence  float64  `json:"confidence"`
}

type Action struct {
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Priority string `json:"priority"`
}

// RootCauseCategories 是允许的根因类别，其它取值归为 unknown。
var RootCauseCategories = []string{"upstream", "account", "gateway", "network", "client", "config", "unknown"}

const (
	maxRootCauses        = 8
	maxActions           = 10
	maxEvidencePerCause  = 12
	maxSummaryRunes      = 2000
	maxTitleRunes        = 200
	maxDetailRunes       = 2000
	fallbackSummaryRunes = 2000
)

var ErrUnparsableFindings = errors.New("logagent: model output is not valid findings JSON")

// rawFindings 容忍模型常见的字段别名与数值格式。
type rawFindings struct {
	Summary          string          `json:"summary"`
	RootCauses       []rawRootCause  `json:"root_causes"`
	LikelyRootCauses []rawRootCause  `json:"likely_root_causes"`
	SuggestedActions []rawAction     `json:"suggested_actions"`
	Actions          []rawAction     `json:"actions"`
	Confidence       json.RawMessage `json:"confidence"`
}

type rawRootCause struct {
	Title       string          `json:"title"`
	Detail      string          `json:"detail"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	EvidenceIDs []string        `json:"evidence_ids"`
	Evidence    []string        `json:"evidence"`
	Confidence  json.RawMessage `json:"confidence"`
}

type rawAction struct {
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Priority string `json:"priority"`
}

// ParseFindings 从模型输出中提取 JSON 并校验。validID 为 nil 时不过滤证据引用。
// 解析失败返回 Fallback 结论与 ErrUnparsableFindings，调用方仍可展示/持久化结论。
func ParseFindings(output string, validID func(string) bool) (Findings, error) {
	payload := extractJSONObject(output)
	var raw rawFindings
	if payload == "" || json.Unmarshal([]byte(payload), &raw) != nil || strings.TrimSpace(raw.Summary) == "" {
		return FallbackFindings(output), ErrUnparsableFindings
	}
	findings := Findings{
		Summary:    clampRunes(MaskText(strings.TrimSpace(raw.Summary)), maxSummaryRunes),
		Confidence: parseConfidence(raw.Confidence),
		RootCauses: []RootCause{},
		Actions:    []Action{},
	}
	causes := raw.RootCauses
	if len(causes) == 0 {
		causes = raw.LikelyRootCauses
	}
	for _, cause := range causes {
		if len(findings.RootCauses) >= maxRootCauses {
			break
		}
		title := strings.TrimSpace(cause.Title)
		detail := strings.TrimSpace(firstNonEmpty(cause.Detail, cause.Description))
		if title == "" && detail == "" {
			continue
		}
		ids := cause.EvidenceIDs
		if len(ids) == 0 {
			ids = cause.Evidence
		}
		findings.RootCauses = append(findings.RootCauses, RootCause{
			Title:       clampRunes(MaskText(title), maxTitleRunes),
			Detail:      clampRunes(MaskText(detail), maxDetailRunes),
			Category:    normalizeCategory(cause.Category),
			EvidenceIDs: filterEvidenceIDs(ids, validID),
			Confidence:  parseConfidence(cause.Confidence),
		})
	}
	actions := raw.SuggestedActions
	if len(actions) == 0 {
		actions = raw.Actions
	}
	for _, action := range actions {
		if len(findings.Actions) >= maxActions {
			break
		}
		title := strings.TrimSpace(action.Title)
		detail := strings.TrimSpace(action.Detail)
		if title == "" && detail == "" {
			continue
		}
		findings.Actions = append(findings.Actions, Action{
			Title:    clampRunes(MaskText(title), maxTitleRunes),
			Detail:   clampRunes(MaskText(detail), maxDetailRunes),
			Priority: normalizePriority(action.Priority),
		})
	}
	return findings, nil
}

// FallbackFindings 把无法解析的输出包装成可展示的兜底结论。
func FallbackFindings(output string) Findings {
	summary := clampRunes(MaskText(strings.TrimSpace(output)), fallbackSummaryRunes)
	return Findings{Summary: summary, RootCauses: []RootCause{}, Actions: []Action{}, Fallback: true}
}

// extractJSONObject 去掉 Markdown 代码围栏，取第一个 '{' 到最后一个 '}'。
func extractJSONObject(output string) string {
	text := strings.TrimSpace(output)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimPrefix(text, "json")
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return ""
	}
	return text[start : end+1]
}

// parseConfidence 接受 0-1 小数、0-100 百分数、数字字符串，结果钳到 [0,1]。
func parseConfidence(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "%")
		if json.Unmarshal([]byte(text), &value) != nil {
			return 0
		}
	}
	if value > 1 && value <= 100 {
		value /= 100
	}
	return min(max(value, 0), 1)
}

func normalizeCategory(category string) string {
	category = strings.ToLower(strings.TrimSpace(category))
	for _, allowed := range RootCauseCategories {
		if category == allowed {
			return category
		}
	}
	return "unknown"
}

func normalizePriority(priority string) string {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case "high", "p0", "p1", "critical", "urgent":
		return "high"
	case "low", "p3":
		return "low"
	default:
		return "medium"
	}
}

func filterEvidenceIDs(ids []string, validID func(string) bool) []string {
	result := []string{}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if validID != nil && !validID(id) {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
		if len(result) >= maxEvidencePerCause {
			break
		}
	}
	return result
}

func clampRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
