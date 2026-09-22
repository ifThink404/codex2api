package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSValidationFailureClassification(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		status   int
		bps      bool
		code     string
		category string
		evidence string
	}{
		{"request validation", 422, true, "", "invalid_request", "http_status"},
		{"server unavailable", 503, true, "", "unavailable", "error_type"},
		{"explicit account code", 422, true, "account_disabled", "account_disabled", "error_code"},
		{"other upstream unchanged", 422, false, "", "unavailable", "error_type"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			d := &UpstreamTransportDiagnostic{ErrorSource: "upstream_http", ErrorStage: "http_response", HTTPStatus: scenario.status, ErrorType: "server_error", ErrorCode: scenario.code}
			if scenario.bps {
				d.BPS = &CodexBPSDiagnostic{Mode: "bps"}
			}
			classifyTransportDiagnostic(d)
			require.Equal(t, scenario.category, d.FailureCategory)
			require.Equal(t, scenario.evidence, d.FailureEvidence)
		})
	}
}
