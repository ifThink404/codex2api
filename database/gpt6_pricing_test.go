package database

import "testing"

func TestGPT6SolAndLunaHaveIndependentPricing(t *testing.T) {
	for _, tc := range []struct {
		model                 string
		input, cached, output float64
	}{
		{"gpt-6-sol", 2, .2, 10}, {"gpt-6-luna", .1, .01, .5},
	} {
		for _, suffix := range []string{"", "-high", "(xhigh)"} {
			model := tc.model + suffix
			if got := PricingManagementModelKey(model); got != tc.model {
				t.Errorf("%s management key=%s", model, got)
			}
			p := GetModelPricing(model)
			assertFloatEqual(t, p.InputPricePerMToken, tc.input)
			assertFloatEqual(t, p.CacheReadPricePerMToken, tc.cached)
			assertFloatEqual(t, p.OutputPricePerMToken, tc.output)
			assertFloatEqual(t, p.LongInputPricePerMToken, tc.input*2)
			assertFloatEqual(t, p.LongOutputPricePerMToken, tc.output*1.5)
		}
	}
}
