package auth

import (
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
)

// Adaptive trust snapshots are attached to the shared Store without changing
// the account scheduler's hot structure. Each replacement publishes a fully
// immutable map, so proxy reads require no database or network round trip.
var promptRiskTrustSnapshots sync.Map // map[*Store]map[string]database.PromptRiskTrustPolicy

func (s *Store) ReplacePromptRiskTrustPolicies(items []*database.PromptRiskTrustPolicy) {
	if s == nil {
		return
	}
	next := make(map[string]database.PromptRiskTrustPolicy, len(items))
	for _, item := range items {
		if item == nil || strings.TrimSpace(item.SubjectKey) == "" {
			continue
		}
		next[item.SubjectKey] = *item
	}
	promptRiskTrustSnapshots.Store(s, next)
}

func (s *Store) GetPromptRiskTrustPolicy(subjectKey string, now time.Time) (database.PromptRiskTrustPolicy, bool) {
	if s == nil || strings.TrimSpace(subjectKey) == "" {
		return database.PromptRiskTrustPolicy{}, false
	}
	raw, ok := promptRiskTrustSnapshots.Load(s)
	if !ok {
		return database.PromptRiskTrustPolicy{}, false
	}
	items, ok := raw.(map[string]database.PromptRiskTrustPolicy)
	if !ok {
		return database.PromptRiskTrustPolicy{}, false
	}
	item, ok := items[subjectKey]
	if !ok || item.Status != database.PromptRiskTrustStatusActive || !item.ValidUntil.After(now.UTC()) || item.LastRiskScore >= item.RiskThreshold {
		return database.PromptRiskTrustPolicy{}, false
	}
	return item, true
}

func (s *Store) RemovePromptRiskTrustPolicy(subjectKey string) {
	if s == nil || strings.TrimSpace(subjectKey) == "" {
		return
	}
	raw, ok := promptRiskTrustSnapshots.Load(s)
	if !ok {
		return
	}
	current, ok := raw.(map[string]database.PromptRiskTrustPolicy)
	if !ok {
		return
	}
	next := make(map[string]database.PromptRiskTrustPolicy, len(current))
	for key, item := range current {
		if key != subjectKey {
			next[key] = item
		}
	}
	promptRiskTrustSnapshots.Store(s, next)
}
