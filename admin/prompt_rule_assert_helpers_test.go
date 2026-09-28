package admin

import (
	"fmt"
	"reflect"
	"testing"
)

// Minimal stdlib assertions for the builtin prompt rule tests (no testify).

func ruleAssertMessage(details []any) string {
	if len(details) == 0 {
		return ""
	}
	return ": " + fmt.Sprint(details...)
}

func ruleRequireNoError(t testing.TB, err error, details ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error %v%s", err, ruleAssertMessage(details))
	}
}

func ruleRequireError(t testing.TB, err error, details ...any) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error%s", ruleAssertMessage(details))
	}
}

func ruleRequireEqual(t testing.TB, want, got any, details ...any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("want %#v, got %#v%s", want, got, ruleAssertMessage(details))
	}
}

func ruleRequireTrue(t testing.TB, value bool, details ...any) {
	t.Helper()
	if !value {
		t.Fatalf("expected true%s", ruleAssertMessage(details))
	}
}

func ruleRequireFalse(t testing.TB, value bool, details ...any) {
	t.Helper()
	if value {
		t.Fatalf("expected false%s", ruleAssertMessage(details))
	}
}

func ruleRequireEmpty(t testing.TB, value any, details ...any) {
	t.Helper()
	if v := reflect.ValueOf(value); value != nil && v.Len() != 0 {
		t.Fatalf("expected empty, got %#v%s", value, ruleAssertMessage(details))
	}
}

func ruleRequireNotEmpty(t testing.TB, value any, details ...any) {
	t.Helper()
	if value == nil || reflect.ValueOf(value).Len() == 0 {
		t.Fatalf("expected non-empty value%s", ruleAssertMessage(details))
	}
}

func ruleRequireLen(t testing.TB, value any, length int, details ...any) {
	t.Helper()
	if got := reflect.ValueOf(value).Len(); got != length {
		t.Fatalf("length = %d, want %d%s", got, length, ruleAssertMessage(details))
	}
}

func ruleRequireZero(t testing.TB, value any, details ...any) {
	t.Helper()
	if value != nil && !reflect.ValueOf(value).IsZero() {
		t.Fatalf("expected zero, got %#v%s", value, ruleAssertMessage(details))
	}
}

func ruleRequireLess(t testing.TB, left, right int, details ...any) {
	t.Helper()
	if left >= right {
		t.Fatalf("%d is not less than %d%s", left, right, ruleAssertMessage(details))
	}
}

func ruleRequireSame(t testing.TB, want, got any, details ...any) {
	t.Helper()
	if reflect.ValueOf(want).Pointer() != reflect.ValueOf(got).Pointer() {
		t.Fatalf("expected the same pointer%s", ruleAssertMessage(details))
	}
}

func ruleRequireNotSame(t testing.TB, want, got any, details ...any) {
	t.Helper()
	if reflect.ValueOf(want).Pointer() == reflect.ValueOf(got).Pointer() {
		t.Fatalf("expected different pointers%s", ruleAssertMessage(details))
	}
}
