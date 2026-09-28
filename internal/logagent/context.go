package logagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/security"
)

// Limits 约束一次分析送进模型的上下文规模。
type Limits struct {
	// MaxInputBytes 是证据上下文的字节上限（约 4 字节/token）。
	MaxInputBytes int
	// MaxRecords 是参与分组的记录数上限，超出部分直接丢弃（Source 一般已按时间倒序）。
	MaxRecords int
}

const (
	DefaultMaxInputBytes = 48 << 10
	MinMaxInputBytes     = 4 << 10
	MaxMaxInputBytes     = 512 << 10
	DefaultMaxRecords    = 200
	MaxMaxRecords        = 1000

	maxMessageBytes     = 1500
	maxBodyBytes        = 4000
	maxFieldValueBytes  = 256
	maxFieldsPerRecord  = 32
	maxSampleIDs        = 8
	maxVaryingValues    = 5
	minTruncatedMessage = 200
)

// Normalize 把零值与越界值收敛到默认/边界。
func (l Limits) Normalize() Limits {
	if l.MaxInputBytes <= 0 {
		l.MaxInputBytes = DefaultMaxInputBytes
	}
	l.MaxInputBytes = min(max(l.MaxInputBytes, MinMaxInputBytes), MaxMaxInputBytes)
	if l.MaxRecords <= 0 {
		l.MaxRecords = DefaultMaxRecords
	}
	l.MaxRecords = min(l.MaxRecords, MaxMaxRecords)
	return l
}

// ContextStats 描述上下文如何被裁剪，随结果返回给管理台。
type ContextStats struct {
	Records        int      `json:"records"`
	DroppedRecords int      `json:"dropped_records"`
	Groups         int      `json:"groups"`
	IncludedGroups int      `json:"included_groups"`
	InputBytes     int      `json:"input_bytes"`
	Truncated      bool     `json:"truncated"`
	EvidenceIDs    []string `json:"evidence_ids"`
}

// EvidenceGroup 是一组同签名的记录，Representative 为组内最新一条（已脱敏）。
type EvidenceGroup struct {
	Label          string              `json:"group"`
	Count          int                 `json:"count"`
	FirstSeen      time.Time           `json:"first_seen"`
	LastSeen       time.Time           `json:"last_seen"`
	EvidenceIDs    []string            `json:"evidence_ids"`
	Kind           string              `json:"kind,omitempty"`
	Status         int                 `json:"status,omitempty"`
	ErrorKind      string              `json:"error_kind,omitempty"`
	Message        string              `json:"message,omitempty"`
	Fields         map[string]string   `json:"fields,omitempty"`
	Varying        map[string][]string `json:"varying,omitempty"`
	Body           string              `json:"body,omitempty"`
	BodyTruncated  bool                `json:"body_truncated,omitempty"`
	signature      string
	varyingOverRun map[string]bool
}

// BuiltContext 是送进模型的证据文本及其统计。
type BuiltContext struct {
	Text  string
	Stats ContextStats
	// validIDs 是出现在上下文中的记录 ID，解析结论时用来过滤模型编造的引用。
	validIDs map[string]struct{}
}

// ValidEvidenceID 报告模型引用的 ID 是否真的出现在上下文里。
func (b *BuiltContext) ValidEvidenceID(id string) bool {
	_, ok := b.validIDs[id]
	return ok
}

// BuildContext 对记录去重分组、脱敏并按字节预算截断。组按出现次数降序、最近时间降序
// 排列，预算不足时先裁组内载荷，再丢弃尾部的组。
func BuildContext(records []Record, limits Limits) *BuiltContext {
	limits = limits.Normalize()
	stats := ContextStats{Records: len(records)}
	if len(records) > limits.MaxRecords {
		stats.DroppedRecords = len(records) - limits.MaxRecords
		records = records[:limits.MaxRecords]
		stats.Truncated = true
	}
	groups := groupRecords(records)
	stats.Groups = len(groups)

	built := &BuiltContext{validIDs: make(map[string]struct{})}
	var b strings.Builder
	budget := limits.MaxInputBytes
	for _, group := range groups {
		line := encodeGroup(group)
		if len(line)+1 > budget-b.Len() {
			// 第一组必须进上下文：逐级裁掉载荷直到放得下。
			if stats.IncludedGroups == 0 {
				line = shrinkGroupToFit(group, budget-1)
			} else {
				line = ""
			}
		}
		if line == "" {
			stats.Truncated = true
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		stats.IncludedGroups++
		for _, id := range group.EvidenceIDs {
			built.validIDs[id] = struct{}{}
			stats.EvidenceIDs = append(stats.EvidenceIDs, id)
		}
	}
	if stats.IncludedGroups < stats.Groups {
		stats.Truncated = true
	}
	built.Text = b.String()
	stats.InputBytes = len(built.Text)
	built.Stats = stats
	return built
}

func groupRecords(records []Record) []*EvidenceGroup {
	bySignature := make(map[string]*EvidenceGroup)
	var ordered []*EvidenceGroup
	for _, record := range records {
		record = maskRecord(record)
		signature := strings.TrimSpace(record.Signature)
		if signature == "" {
			signature = RecordSignature(record)
		}
		group, exists := bySignature[signature]
		if !exists {
			group = &EvidenceGroup{
				signature:      signature,
				Kind:           record.Kind,
				Status:         record.Status,
				ErrorKind:      record.ErrorKind,
				Message:        record.Message,
				Fields:         record.Fields,
				Body:           record.Body,
				FirstSeen:      record.Time,
				LastSeen:       record.Time,
				varyingOverRun: map[string]bool{},
			}
			bySignature[signature] = group
			ordered = append(ordered, group)
		} else {
			if record.Time.After(group.LastSeen) {
				// 代表记录取组内最新一条；旧代表的字段差异计入 varying。
				previous := group.Fields
				group.Message, group.Fields = record.Message, record.Fields
				if record.Body != "" {
					group.Body = record.Body
				}
				group.LastSeen = record.Time
				group.noteVarying(previous)
			} else {
				group.noteVarying(record.Fields)
			}
			if !record.Time.IsZero() && (group.FirstSeen.IsZero() || record.Time.Before(group.FirstSeen)) {
				group.FirstSeen = record.Time
			}
		}
		group.Count++
		if record.ID != "" && len(group.EvidenceIDs) < maxSampleIDs {
			group.EvidenceIDs = append(group.EvidenceIDs, record.ID)
		}
	}
	for _, group := range ordered {
		group.finishVarying()
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Count != ordered[j].Count {
			return ordered[i].Count > ordered[j].Count
		}
		return ordered[i].LastSeen.After(ordered[j].LastSeen)
	})
	for i, group := range ordered {
		group.Label = fmt.Sprintf("G%d", i+1)
	}
	return ordered
}

// noteVarying 记录非代表记录与代表记录不同的字段取值，便于模型看出"影响多个账号"之类的分布。
func (g *EvidenceGroup) noteVarying(fields map[string]string) {
	for key, value := range fields {
		if value == "" || g.Fields[key] == value {
			continue
		}
		if g.Varying == nil {
			g.Varying = make(map[string][]string)
		}
		values := g.Varying[key]
		if containsString(values, value) {
			continue
		}
		if len(values) >= maxVaryingValues {
			g.varyingOverRun[key] = true
			continue
		}
		g.Varying[key] = append(values, value)
	}
}

func (g *EvidenceGroup) finishVarying() {
	for key, values := range g.Varying {
		// 代表记录的取值也列进去，避免只看 varying 时漏掉它。
		if current := g.Fields[key]; current != "" && !containsString(values, current) {
			values = append([]string{current}, values...)
		}
		if g.varyingOverRun[key] {
			values = append(values, "…")
		}
		g.Varying[key] = values
	}
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func encodeGroup(group *EvidenceGroup) string {
	payload, err := json.Marshal(group)
	if err != nil {
		return ""
	}
	return string(payload)
}

// shrinkGroupToFit 依次去掉 body、varying、fields 并截短消息，直到编码后不超过 budget。
func shrinkGroupToFit(group *EvidenceGroup, budget int) string {
	copyGroup := *group
	steps := []func(){
		func() { copyGroup.Body, copyGroup.BodyTruncated = "", group.Body != "" },
		func() { copyGroup.Varying = nil },
		func() { copyGroup.Fields = nil },
		func() {
			copyGroup.Message = truncateBytes(copyGroup.Message, max(minTruncatedMessage, budget/2))
		},
		func() { copyGroup.Message = truncateBytes(copyGroup.Message, minTruncatedMessage) },
	}
	for _, step := range steps {
		step()
		if line := encodeGroup(&copyGroup); len(line) <= budget {
			return line
		}
	}
	return ""
}

var (
	normalizeUUIDPattern   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	normalizeHexPattern    = regexp.MustCompile(`(?i)\b[0-9a-f]{12,}\b`)
	normalizeNumberPattern = regexp.MustCompile(`\d+`)
)

// RecordSignature 计算默认分组签名：状态码、错误类型、归一化消息（ID/数字抹平）、
// endpoint 与 model 相同即视为同一类错误。
func RecordSignature(record Record) string {
	message := strings.Join(strings.Fields(record.Message), " ")
	message = normalizeUUIDPattern.ReplaceAllString(message, "<id>")
	message = normalizeHexPattern.ReplaceAllString(message, "<hex>")
	message = normalizeNumberPattern.ReplaceAllString(message, "#")
	parts := []string{
		record.Kind,
		fmt.Sprint(record.Status),
		strings.TrimSpace(record.ErrorKind),
		truncateBytes(message, 512),
		record.Fields["endpoint"],
		record.Fields["upstream_endpoint"],
		record.Fields["model"],
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:12])
}

var (
	jwtPattern   = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	emailPattern = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)
)

// identifierFieldKeys 是网关自己生成的关联 ID：只做 URL 凭据脱敏，保留原值便于模型
// 对齐同一请求的多次尝试（MaskSensitiveData 会把 UUID 形态的值整体抹掉）。
var identifierFieldKeys = map[string]bool{
	"request_id":          true,
	"upstream_request_id": true,
	"parent_request_id":   true,
}

// MaskText 对不可信文本做脱敏：复用 security.MaskSensitiveData（token/密钥/UUID/URL 凭据），
// 再补上 JWT 与邮箱。
func MaskText(text string) string {
	if text == "" {
		return text
	}
	text = security.MaskSensitiveData(text)
	text = jwtPattern.ReplaceAllString(text, "****JWT-MASKED****")
	return emailPattern.ReplaceAllStringFunc(text, security.MaskEmail)
}

func maskRecord(record Record) Record {
	record.Message = truncateBytes(MaskText(record.Message), maxMessageBytes)
	record.ErrorKind = truncateBytes(MaskText(record.ErrorKind), maxFieldValueBytes)
	record.Body = truncateBytes(MaskText(record.Body), maxBodyBytes)
	if len(record.Fields) > 0 {
		keys := make([]string, 0, len(record.Fields))
		for key := range record.Fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		masked := make(map[string]string, min(len(keys), maxFieldsPerRecord))
		for _, key := range keys {
			if len(masked) >= maxFieldsPerRecord {
				break
			}
			value := strings.TrimSpace(record.Fields[key])
			if value == "" {
				continue
			}
			switch {
			case identifierFieldKeys[key]:
				value = security.MaskURLCredentials(value)
			case strings.Contains(key, "email"):
				value = security.MaskEmail(value)
			default:
				value = MaskText(value)
			}
			masked[truncateBytes(key, 64)] = truncateBytes(value, maxFieldValueBytes)
		}
		record.Fields = masked
	}
	return record
}

// truncateBytes 按字节截断且不切断 UTF-8 字符，截断时追加省略号。
func truncateBytes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	const ellipsis = "…"
	cut := max(limit-len(ellipsis), 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + ellipsis
}
