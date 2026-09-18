package auth

import "testing"

func TestNormalizeAccountPolicy(t *testing.T) {
	cases := []struct{ field, in, want string }{
		{"prompt_filter_policy", "", PolicyInherit},
		{"prompt_filter_policy", "EXEMPT ", PromptFilterPolicyExempt},
		{"prompt_filter_policy", "off", PolicyInherit}, // wrong enum for this field → inherit
		{"egress_policy", "direct", EgressPolicyDirect},
		{"egress_policy", "exempt", PolicyInherit},
		{"session_guards_policy", "Off", SessionGuardsPolicyOff},
		{"session_guards_policy", "direct", PolicyInherit},
		{"unknown_field", "direct", PolicyInherit},
	}
	for _, tc := range cases {
		if got := NormalizeAccountPolicy(tc.field, tc.in); got != tc.want {
			t.Fatalf("%s %q: got %q want %q", tc.field, tc.in, got, tc.want)
		}
	}
	if err := ValidateAccountPolicy("egress_policy")("pool"); err == nil {
		t.Fatal("expected validation error for egress_policy=pool")
	}
	if err := ValidateAccountPolicy("egress_policy")("direct"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	acc := &Account{EgressPolicy: EgressPolicyDirect, PromptFilterPolicy: PromptFilterPolicyExempt, SessionGuardsPolicy: SessionGuardsPolicyOff}
	if !acc.EgressDirect() || !acc.PromptFilterExempt() || !acc.SessionGuardsOff() {
		t.Fatal("predicates must reflect the policy fields")
	}
	if (&Account{}).EgressDirect() || (&Account{}).PromptFilterExempt() || (&Account{}).SessionGuardsOff() {
		t.Fatal("zero value must be inherit")
	}
}
