package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPromptIntelligenceAIEvidenceAndAdvancedConfigCAS(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "prompt-intelligence-ai.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id, prompt_filter_advanced_config) VALUES (1, '{}') ON CONFLICT(id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	candidate, _, err := db.StagePromptRuleCandidate(ctx, PromptRuleCandidateInput{
		Fingerprint: strings.Repeat("a", 64), Kind: PromptRuleCandidateKindEvidence,
		Source: PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: "redacted CY evidence",
	}, PromptRuleCandidateEvidenceInput{
		SourceKind: PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: "incident-1",
		SourceRefHash: strings.Repeat("b", 64), ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := PromptRuleCandidateEvidenceInput{
		SourceKind: PromptRuleCandidateSourceAIAnalysis, SourceRef: "analysis-1",
		SourceRefHash: strings.Repeat("c", 64), MetadataJSON: `{"decision":"identity"}`, ObservedAt: time.Now().UTC(),
	}
	evidence, added, err := db.AddPromptRuleCandidateEvidence(ctx, candidate.ID, input)
	if err != nil || !added || evidence.CandidateID != candidate.ID {
		t.Fatalf("add evidence=%#v added=%v err=%v", evidence, added, err)
	}
	replayed, added, err := db.AddPromptRuleCandidateEvidence(ctx, candidate.ID, input)
	if err != nil || added || replayed.ID != evidence.ID {
		t.Fatalf("replay evidence=%#v added=%v err=%v", replayed, added, err)
	}

	revisionInput := PromptRuleCandidateEvidenceInput{
		SourceKind: PromptRuleCandidateSourceAIIdentityUpdate, SourceRef: "analysis-1",
		SourceRefHash: strings.Repeat("d", 64), MetadataJSON: `{"version":1}`, ObservedAt: time.Now().UTC(),
	}
	swapped, revision, err := db.CompareAndSwapPromptFilterAdvancedConfigWithEvidence(ctx, candidate.ID, "{}", `{"review_adapter":{"system_prompt":"managed"}}`, revisionInput)
	if err != nil || !swapped || revision == nil || revision.SourceKind != PromptRuleCandidateSourceAIIdentityUpdate {
		t.Fatalf("CAS swapped=%v revision=%#v err=%v", swapped, revision, err)
	}
	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || !strings.Contains(persisted.PromptFilterAdvancedConfig, "managed") {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	conflictInput := revisionInput
	conflictInput.SourceRefHash = strings.Repeat("e", 64)
	swapped, revision, err = db.CompareAndSwapPromptFilterAdvancedConfigWithEvidence(ctx, candidate.ID, "{}", `{"review_adapter":{"system_prompt":"lost"}}`, conflictInput)
	if err != nil || swapped || revision != nil {
		t.Fatalf("stale CAS swapped=%v revision=%#v err=%v", swapped, revision, err)
	}
	item, err := db.GetPromptRuleCandidate(ctx, candidate.ID)
	if err != nil || item.EvidenceCount != 3 {
		t.Fatalf("candidate=%#v err=%v", item, err)
	}
}
