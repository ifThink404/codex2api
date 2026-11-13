package auth

import "testing"

func TestNormalizeAccountPolicy(t *testing.T) {
	cases := []struct{ field, in, want string }{
		{"egress_policy", "", PolicyInherit},
		{"egress_policy", "direct", EgressPolicyDirect},
		{"egress_policy", " DIRECT ", EgressPolicyDirect},
		{"egress_policy", "exempt", PolicyInherit},
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
	if !(&Account{EgressPolicy: EgressPolicyDirect}).EgressDirect() {
		t.Fatal("EgressDirect must reflect the policy field")
	}
	if (&Account{}).EgressDirect() {
		t.Fatal("zero value must be inherit")
	}
}
