package database

import (
	"context"
	"testing"
	"time"
)

func TestPromptRiskAdaptiveTrustLifecycleAndAutomaticSuspension(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "local_filter", Action: "allow", ReviewModel: "review-model", ReviewFlagged: false,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "gateway-a", NewAPIUserID: "trusted-user",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog(clean): %v", err)
	}
	subjectKey := PromptRiskNewAPIUserSubjectKey("gateway-a", "trusted-user")
	policy, err := db.UpsertPromptRiskTrustPolicy(ctx, PromptRiskTrustPolicyInput{
		SubjectType: PromptRiskSubjectNewAPIUser, SubjectKey: subjectKey, Reason: "低风险付费用户首字优化",
		RiskThreshold: 35, ValidUntil: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil || policy.Status != PromptRiskTrustStatusActive {
		t.Fatalf("UpsertPromptRiskTrustPolicy policy=%#v err=%v", policy, err)
	}
	if err := db.RecordPromptRiskTrustBypass(ctx, policy.ID, policy.SubjectType, policy.SubjectKey, "request-hash"); err != nil {
		t.Fatalf("RecordPromptRiskTrustBypass: %v", err)
	}
	policy, err = db.GetPromptRiskTrustPolicy(ctx, policy.SubjectType, policy.SubjectKey)
	if err != nil || policy.BypassCount != 1 || policy.LastBypassAt == nil {
		t.Fatalf("bypass audit policy=%#v err=%v", policy, err)
	}
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "local_filter", Action: "block", Score: 90, AuditScore: 90, StrikeEligible: true,
		MatchedPatterns: `[{"name":"terminal","weight":90}]`, NewAPIPolicyStatus: "verified",
		NewAPIPlatform: "gateway-a", NewAPIUserID: "trusted-user",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog(block): %v", err)
	}
	active, err := db.ReconcilePromptRiskTrustPolicies(ctx)
	if err != nil {
		t.Fatalf("ReconcilePromptRiskTrustPolicies: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("high-risk policy remained active: %#v", active)
	}
	policy, err = db.GetPromptRiskTrustPolicy(ctx, policy.SubjectType, policy.SubjectKey)
	if err != nil || policy.Status != PromptRiskTrustStatusSuspended || policy.LastRiskScore < 35 {
		t.Fatalf("automatic suspension policy=%#v err=%v", policy, err)
	}
	if err := db.RecordPromptRiskTrustBypass(ctx, policy.ID, policy.SubjectType, policy.SubjectKey, "late-request-hash"); err != nil {
		t.Fatalf("RecordPromptRiskTrustBypass(suspended): %v", err)
	}
	policy, err = db.GetPromptRiskTrustPolicy(ctx, policy.SubjectType, policy.SubjectKey)
	if err != nil || policy.BypassCount != 1 {
		t.Fatalf("suspended policy accepted a late bypass: policy=%#v err=%v", policy, err)
	}
	events, err := db.ListPromptRiskTrustEvents(ctx, policy.SubjectType, policy.SubjectKey, 20)
	if err != nil || len(events) < 3 || events[0].EventType != PromptRiskTrustEventAutoSuspended {
		t.Fatalf("trust audit events=%#v err=%v", events, err)
	}
}

func TestPromptRiskAdaptiveTrustRejectsNonPersonAndPermanentGrant(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if _, err := db.UpsertPromptRiskTrustPolicy(ctx, PromptRiskTrustPolicyInput{
		SubjectType: PromptRiskSubjectSession, SubjectKey: "session", Reason: "invalid", RiskThreshold: 35,
		ValidUntil: time.Now().UTC().Add(time.Hour),
	}); err == nil {
		t.Fatal("non-person trust grant unexpectedly succeeded")
	}
	if _, err := db.UpsertPromptRiskTrustPolicy(ctx, PromptRiskTrustPolicyInput{
		SubjectType: PromptRiskSubjectNewAPIUser, SubjectKey: "person", Reason: "too long", RiskThreshold: 35,
		ValidUntil: time.Now().UTC().Add(31 * 24 * time.Hour),
	}); err == nil {
		t.Fatal("permanent-like trust grant unexpectedly succeeded")
	}
}
