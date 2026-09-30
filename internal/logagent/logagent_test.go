package logagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeLLM struct {
	output       string
	err          error
	usage        Usage
	model        string
	instructions string
	input        string
}

func (f *fakeLLM) Respond(ctx context.Context, model, instructions, input string) (string, error) {
	f.model, f.instructions, f.input = model, instructions, input
	return f.output, f.err
}

type fakeUsageLLM struct{ fakeLLM }

func (f *fakeUsageLLM) RespondWithUsage(ctx context.Context, model, instructions, input string) (string, Usage, error) {
	output, err := f.Respond(ctx, model, instructions, input)
	return output, f.usage, err
}

func errorRecord(id string, at time.Time, account, message string) Record {
	return Record{
		ID:        id,
		Kind:      "usage_log",
		Time:      at,
		Status:    502,
		ErrorKind: "upstream_error",
		Message:   message,
		Fields:    map[string]string{"endpoint": "/v1/responses", "model": "gpt-5", "account": account},
	}
}

func TestBuildContextGroupsSimilarErrors(t *testing.T) {
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	records := []Record{
		errorRecord("usage:1", base, "acct-a", "upstream timeout after 30000ms (req 4f1c2d3e-aaaa-bbbb-cccc-0123456789ab)"),
		errorRecord("usage:2", base.Add(time.Minute), "acct-b", "upstream timeout after 31000ms (req 5f1c2d3e-aaaa-bbbb-cccc-0123456789ab)"),
		errorRecord("usage:3", base.Add(2*time.Minute), "acct-c", "upstream timeout after 29000ms (req 6f1c2d3e-aaaa-bbbb-cccc-0123456789ab)"),
		{ID: "usage:4", Kind: "usage_log", Time: base, Status: 429, Message: "rate limited", Fields: map[string]string{"model": "gpt-5"}},
	}
	built := BuildContext(records, Limits{})
	if built.Stats.Groups != 2 || built.Stats.IncludedGroups != 2 {
		t.Fatalf("stats = %+v, want 2 groups", built.Stats)
	}
	lines := strings.Split(strings.TrimSpace(built.Text), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", len(lines), built.Text)
	}
	first := lines[0]
	for _, want := range []string{`"group":"G1"`, `"count":3`, `"usage:1"`, `"usage:3"`, `"acct-c"`, `"varying"`, `"acct-a"`} {
		if !strings.Contains(first, want) {
			t.Fatalf("first group missing %s: %s", want, first)
		}
	}
	if !strings.Contains(lines[1], `"count":1`) || !strings.Contains(lines[1], `"status":429`) {
		t.Fatalf("second group = %s", lines[1])
	}
	for _, id := range []string{"usage:1", "usage:2", "usage:3", "usage:4"} {
		if !built.ValidEvidenceID(id) {
			t.Fatalf("expected %s to be valid evidence", id)
		}
	}
	if built.ValidEvidenceID("usage:999") {
		t.Fatal("unknown id must not be valid")
	}
}

func TestBuildContextRespectsByteBudget(t *testing.T) {
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	var records []Record
	for i := range 200 {
		records = append(records, Record{
			ID:      fmt.Sprintf("r%d", i),
			Time:    base.Add(time.Duration(i) * time.Second),
			Status:  500,
			Message: fmt.Sprintf("distinct failure kind %c%c", 'a'+i%26, 'a'+i/26),
			Body:    strings.Repeat("x", 3000),
		})
	}
	limits := Limits{MaxInputBytes: MinMaxInputBytes, MaxRecords: 150}
	built := BuildContext(records, limits)
	if built.Stats.InputBytes > MinMaxInputBytes {
		t.Fatalf("input bytes %d exceed budget %d", built.Stats.InputBytes, MinMaxInputBytes)
	}
	if !built.Stats.Truncated || built.Stats.DroppedRecords != 50 {
		t.Fatalf("stats = %+v, want truncated with 50 dropped", built.Stats)
	}
	if built.Stats.IncludedGroups == 0 || built.Stats.IncludedGroups >= built.Stats.Groups {
		t.Fatalf("included %d of %d groups", built.Stats.IncludedGroups, built.Stats.Groups)
	}
}

func TestBuildContextShrinksOversizedFirstGroup(t *testing.T) {
	records := []Record{{ID: "only", Status: 500, Message: strings.Repeat("m", 1400), Body: strings.Repeat("b", 3900),
		Fields: map[string]string{"endpoint": strings.Repeat("e", 250)}}}
	built := BuildContext(records, Limits{MaxInputBytes: MinMaxInputBytes})
	if built.Stats.IncludedGroups != 1 || built.Stats.InputBytes > MinMaxInputBytes {
		t.Fatalf("stats = %+v", built.Stats)
	}
	if !strings.Contains(built.Text, `"body_truncated":true`) {
		t.Fatalf("expected body to be dropped: %s", built.Text)
	}
}

func TestBuildContextMasksSecrets(t *testing.T) {
	records := []Record{{
		ID:      "cap:1",
		Status:  401,
		Message: "invalid Authorization: Bearer sk-abcdefghijklmnopqrstuvwxyz0123 for alice@example.com",
		Body:    `{"access_token":"tok-secret-value","id":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk"}`,
		Fields: map[string]string{
			"account_email": "bob@example.org",
			"proxy":         "http://user:pass@10.0.0.1:8080",
			"request_id":    "4f1c2d3e-aaaa-bbbb-cccc-0123456789ab",
		},
	}}
	text := BuildContext(records, Limits{}).Text
	for _, leaked := range []string{"sk-abcdefghijklmnopqrstuvwxyz0123", "alice@example.com", "bob@example.org", "tok-secret-value", "eyJhbGciOiJIUzI1NiJ9", "user:pass"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("context leaked %q: %s", leaked, text)
		}
	}
	if !strings.Contains(text, "4f1c2d3e-aaaa-bbbb-cccc-0123456789ab") {
		t.Fatalf("request id should be preserved: %s", text)
	}
}

func TestParseFindingsValidatesAndFilters(t *testing.T) {
	output := "```json\n" + `{
		"summary": "Upstream timeouts on one proxy",
		"root_causes": [
			{"title": "Proxy saturation", "detail": "all failures via proxy p1", "category": "Network", "evidence_ids": ["usage:1", "usage:999", "usage:1"], "confidence": 85},
			{"title": "", "detail": ""},
			{"title": "Other", "category": "weird", "evidence": ["usage:2"], "confidence": "0.3"}
		],
		"actions": [{"title": "Rotate proxy", "priority": "P1"}, {"title": "Watch", "priority": "whenever"}],
		"confidence": 0.7
	}` + "\n```"
	valid := map[string]bool{"usage:1": true, "usage:2": true}
	findings, err := ParseFindings(output, func(id string) bool { return valid[id] })
	if err != nil {
		t.Fatalf("ParseFindings err = %v", err)
	}
	if findings.Fallback || findings.Summary != "Upstream timeouts on one proxy" || findings.Confidence != 0.7 {
		t.Fatalf("findings = %+v", findings)
	}
	if len(findings.RootCauses) != 2 {
		t.Fatalf("root causes = %+v", findings.RootCauses)
	}
	first := findings.RootCauses[0]
	if first.Category != "network" || first.Confidence != 0.85 || strings.Join(first.EvidenceIDs, ",") != "usage:1" {
		t.Fatalf("first cause = %+v", first)
	}
	second := findings.RootCauses[1]
	if second.Category != "unknown" || second.Confidence != 0.3 || strings.Join(second.EvidenceIDs, ",") != "usage:2" {
		t.Fatalf("second cause = %+v", second)
	}
	if len(findings.Actions) != 2 || findings.Actions[0].Priority != "high" || findings.Actions[1].Priority != "medium" {
		t.Fatalf("actions = %+v", findings.Actions)
	}
}

func TestParseFindingsFallback(t *testing.T) {
	for _, output := range []string{"", "I think the upstream is down, token sk-abcdefghijklmnopqrstuvwxyz0123", `{"root_causes": []}`, "{not json}"} {
		findings, err := ParseFindings(output, nil)
		if !errors.Is(err, ErrUnparsableFindings) {
			t.Fatalf("output %q: err = %v", output, err)
		}
		if !findings.Fallback || findings.RootCauses == nil || findings.Actions == nil {
			t.Fatalf("output %q: findings = %+v", output, findings)
		}
		if strings.Contains(findings.Summary, "sk-abcdefghijklmnopqrstuvwxyz0123") {
			t.Fatalf("fallback leaked secret: %q", findings.Summary)
		}
	}
}

func TestAnalyzeWithFakeLLM(t *testing.T) {
	llm := &fakeUsageLLM{fakeLLM{
		output: `{"summary":"s","root_causes":[{"title":"t","evidence_ids":["usage:1"],"confidence":0.9}],"suggested_actions":[],"confidence":0.5}`,
		usage:  Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}}
	records := []Record{errorRecord("usage:1", time.Now(), "a", "boom")}
	result, err := Analyze(context.Background(), llm, Request{Source: "ops_errors", Model: "gpt-5", Records: records, Focus: "why?", Language: "zh"})
	if err != nil {
		t.Fatalf("Analyze err = %v", err)
	}
	if result.Usage.TotalTokens != 15 || result.ParseError != "" || result.Findings.RootCauses[0].EvidenceIDs[0] != "usage:1" {
		t.Fatalf("result = %+v", result)
	}
	if llm.model != "gpt-5" || !strings.Contains(llm.instructions, "Simplified Chinese") {
		t.Fatalf("model=%q instructions=%q", llm.model, llm.instructions)
	}
	for _, want := range []string{"Source: ops_errors", "Operator question: why?", "<evidence>", `"usage:1"`} {
		if !strings.Contains(llm.input, want) {
			t.Fatalf("input missing %q:\n%s", want, llm.input)
		}
	}
}

func TestAnalyzeErrors(t *testing.T) {
	records := []Record{errorRecord("usage:1", time.Now(), "a", "boom")}
	if _, err := Analyze(context.Background(), nil, Request{Model: "m", Records: records}); !errors.Is(err, ErrNoLLM) {
		t.Fatalf("nil llm err = %v", err)
	}
	if _, err := Analyze(context.Background(), &fakeLLM{}, Request{Records: records}); !errors.Is(err, ErrNoModel) {
		t.Fatalf("no model err = %v", err)
	}
	if _, err := Analyze(context.Background(), &fakeLLM{}, Request{Model: "m"}); !errors.Is(err, ErrNoRecords) {
		t.Fatalf("no records err = %v", err)
	}
	upstream := errors.New("HTTP 503")
	if _, err := Analyze(context.Background(), &fakeLLM{err: upstream}, Request{Model: "m", Records: records}); !errors.Is(err, upstream) {
		t.Fatalf("llm err = %v", err)
	}
	result, err := Analyze(context.Background(), &fakeLLM{output: "plain text"}, Request{Model: "m", Records: records})
	if err != nil || result.ParseError == "" || !result.Findings.Fallback {
		t.Fatalf("fallback result = %+v err = %v", result, err)
	}
}

func TestRegistry(t *testing.T) {
	registry := NewRegistry()
	src := SourceFunc("bps.captures", func(ctx context.Context, q Query) ([]Record, error) { return nil, nil })
	if err := registry.Register(src); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(src); !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("duplicate err = %v", err)
	}
	if err := registry.Register(SourceFunc("Bad Name", nil)); !errors.Is(err, ErrInvalidSourceName) {
		t.Fatalf("invalid err = %v", err)
	}
	if _, ok := registry.Lookup("bps.captures"); !ok || strings.Join(registry.Names(), ",") != "bps.captures" {
		t.Fatalf("names = %v", registry.Names())
	}
}
