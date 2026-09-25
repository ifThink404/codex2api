package auth

import "strings"

// DispatchPolicy selects which usage windows fence a request.
// Standard models keep the existing 5h/7d account-level gates.
// Spark requests ignore those gates and only look at the independent spark window.
type DispatchPolicy struct {
	spark bool
	model string
}

var (
	DispatchPolicyStandard = DispatchPolicy{}
	DispatchPolicySpark    = DispatchPolicy{spark: true}
)

func (p DispatchPolicy) IsSpark() bool { return p.spark }

func (p DispatchPolicy) Model() string { return p.model }

// The model travels with this request, never as mutable account-global state.
func (p DispatchPolicy) WithModel(model string) DispatchPolicy {
	p.model = strings.ToLower(strings.TrimSpace(model))
	return p
}
