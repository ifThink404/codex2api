package proxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestLegacyWindowIsOpaqueAndDoesNotInferSequence(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexWindowIDHeader, continuityTestThread)
	thread, number, known, reason := parseContinuityWindow(headers, nil, false)
	require.Empty(t, thread)
	require.Zero(t, number)
	require.False(t, known)
	require.Equal(t, "window_legacy", reason)
	_, err := codexAccountWindowInputs(headers, nil)
	require.Error(t, err, "strict mapping still requires sequenced windows")
	windows, err := codexAccountWindowInputs(headers, nil, true)
	require.NoError(t, err)
	require.Empty(t, windows, "never fabricate a :0 baseline")
	headers.Set(codexTurnMetadataHeader, `{"window_number":2}`)
	_, _, known, reason = parseContinuityWindow(headers, nil, false)
	require.False(t, known)
	require.Equal(t, "number_conflict", reason)
	_, err = codexAccountWindowInputs(headers, nil, true)
	require.Error(t, err)
}

func TestRelaxedLegacyWindowKeepsScopeAndContextGuards(t *testing.T) {
	for _, scenario := range []string{"strict", "thread_conflict", "empty_input", "number_conflict"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, _, key, c, body := relaxedOwnerRecoverySetup(t, true, true, "off")
			c.Request.Header.Set(codexWindowIDHeader, continuityTestThread)
			switch scenario {
			case "strict":
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
					s.CodexForkAccountFallbackEnabled = false
					s.CodexSessionFailoverEnabled = true
					return s
				})
			case "thread_conflict":
				c.Request.Header.Set(codexThreadIDHeader, "01a03bb0-9da5-7772-a16a-f38258dd30c5")
			case "empty_input":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[]`))
			case "number_conflict":
				c.Request.Header.Set(codexTurnMetadataHeader, `{"window_number":2}`)
			}
			c.Set(ingressRequestBodyContextKey, body)
			require.NotNil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, owner.ID(), record.AccountID)
			require.Zero(t, record.FailoverCount)
		})
	}
}
