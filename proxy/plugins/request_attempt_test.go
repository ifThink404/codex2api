package plugins

import "testing"

func TestUsageValuesApplyOnlyToTheirAttempt(t *testing.T) {
	req := NewRequest("req-attempts", KindResponses, nil, nil, 0)
	req.beginAttempt()
	req.SetUsageErrorMessage("provider text of attempt 1")
	req.SetUsageErrorKind("p", "kind of attempt 1")
	if req.UsageErrorMessage() != "provider text of attempt 1" || req.UsageErrorKind("p") != "kind of attempt 1" {
		t.Fatal("values must apply to the attempt that set them")
	}
	req.beginAttempt()
	if got := req.UsageErrorMessage(); got != "" {
		t.Fatalf("attempt 2 inherited the error message %q", got)
	}
	if got := req.UsageErrorKind("p"); got != "" {
		t.Fatalf("attempt 2 inherited the error kind %q", got)
	}
	req.SetUsageErrorMessage("provider text of attempt 2")
	if req.UsageErrorMessage() != "provider text of attempt 2" {
		t.Fatal("attempt 2 records its own message")
	}
}
