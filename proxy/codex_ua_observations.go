package proxy

import (
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// Optional diagnostics only; this index never feeds outbound profile selection.
type CodexUserAgentObservation struct {
	Status                string     `json:"status"`
	Source                string     `json:"source"`
	LogLimit              int        `json:"log_limit"`
	CheckedAt             time.Time  `json:"checked_at"`
	SampleCount           int64      `json:"sample_count"`
	MatchCount            int64      `json:"match_count"`
	LastSeenAt            *time.Time `json:"last_seen_at,omitempty"`
	VersionPairCount      int64      `json:"version_pair_count"`
	VersionPairLastSeenAt *time.Time `json:"version_pair_last_seen_at,omitempty"`
	Warnings              []string   `json:"warnings,omitempty"`
}

type codexUAObservedCount struct {
	count    int64
	lastSeen time.Time
}

type codexUAObservedClient struct {
	count  int64
	fields map[string]map[string]codexUAObservedCount
}

type CodexUserAgentObservations struct {
	checkedAt time.Time
	clients   map[string]*codexUAObservedClient
}

var codexObservedUA = regexp.MustCompile(`^([^/\r\n]+)/([^\s()]+)\s+\(([^();]+);\s*([^()]+)\)\s+([^()\r\n]+)\s+\(([^();]+);\s*([^()]+)\)$`)

func codexObservedUAFields(ua string) (string, map[string]string, bool) {
	if len(ua) > 2048 || strings.ContainsAny(ua, "\r\n\x00") {
		return "", nil, false
	}
	parts := codexObservedUA.FindStringSubmatch(strings.TrimSpace(ua))
	if len(parts) != 8 {
		return "", nil, false
	}
	for i := 1; i < len(parts); i++ {
		parts[i] = strings.TrimSpace(parts[i])
	}
	client := strings.ToLower(parts[1])
	fields := map[string]string{
		"platform":     strings.ToLower(parts[3]) + "\x00" + strings.ToLower(parts[4]),
		"terminal":     parts[5],
		"app_name":     strings.ToLower(parts[6]),
		"version_pair": parts[2] + "\x00" + strings.ToLower(parts[6]) + "\x00" + parts[7],
	}
	fields["combination"] = fields["platform"] + "\x00" + fields["terminal"] + "\x00" + fields["version_pair"]
	return client, fields, true
}

func NewCodexUserAgentObservations(samples []database.UsageClientUserAgentSample, checkedAt time.Time) *CodexUserAgentObservations {
	index := &CodexUserAgentObservations{checkedAt: checkedAt, clients: make(map[string]*codexUAObservedClient)}
	for _, sample := range samples {
		name, fields, ok := codexObservedUAFields(sample.UserAgent)
		if !ok || sample.Count <= 0 {
			continue
		}
		client := index.clients[name]
		if client == nil {
			client = &codexUAObservedClient{fields: make(map[string]map[string]codexUAObservedCount)}
			index.clients[name] = client
		}
		client.count += sample.Count
		for field, value := range fields {
			if client.fields[field] == nil {
				client.fields[field] = make(map[string]codexUAObservedCount)
			}
			stat := client.fields[field][value]
			stat.count += sample.Count
			if sample.LastSeen.After(stat.lastSeen) {
				stat.lastSeen = sample.LastSeen
			}
			client.fields[field][value] = stat
		}
	}
	return index
}

func (index *CodexUserAgentObservations) observe(ua string) *CodexUserAgentObservation {
	result := &CodexUserAgentObservation{Source: "usage_logs.client_user_agent", LogLimit: database.UsageClientUserAgentSampleLimit, Status: "unavailable"}
	if index == nil {
		return result
	}
	result.CheckedAt = index.checkedAt
	name, fields, ok := codexObservedUAFields(ua)
	if !ok {
		result.Status = "unparseable"
		return result
	}
	client := index.clients[name]
	if client == nil {
		result.Status = "empty"
		return result
	}
	result.SampleCount = client.count
	pair := client.fields["version_pair"][fields["version_pair"]]
	result.VersionPairCount = pair.count
	if pair.count > 0 {
		result.VersionPairLastSeenAt = &pair.lastSeen
	}
	full := client.fields["combination"][fields["combination"]]
	result.MatchCount = full.count
	if full.count > 0 {
		result.Status, result.LastSeenAt = "matched", &full.lastSeen
		return result
	}
	result.Status = "unseen"
	for _, field := range []string{"terminal", "app_name", "platform", "version_pair"} {
		if client.fields[field][fields[field]].count == 0 {
			result.Warnings = append(result.Warnings, field)
		}
	}
	if len(result.Warnings) == 0 {
		result.Warnings = []string{"combination"}
	}
	return result
}

func (index *CodexUserAgentObservations) Apply(preview *CodexUserAgentPreview) {
	preview.Warnings = nil
	if preview.Persona != nil {
		preview.Persona.Observation = index.observe(preview.Persona.UserAgent)
		preview.Warnings = preview.Persona.Observation.Warnings
	}
	for i := range preview.Samples {
		preview.Samples[i].Observation = index.observe(preview.Samples[i].UserAgent)
	}
}
