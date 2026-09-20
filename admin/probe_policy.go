package admin

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/codex2api/auth"
)

func parseProbePolicyCredentials(modeRaw, intervalRaw json.RawMessage, updates map[string]interface{}) error {
	if len(modeRaw) > 0 {
		var mode string
		if json.Unmarshal(modeRaw, &mode) != nil || (mode != "auto" && mode != "off" && mode != "on") {
			return fmt.Errorf("probe_mode must be auto, off, or on")
		}
		updates[auth.ProbeModeCredentialKey] = mode
	}
	if len(intervalRaw) > 0 {
		var minutes int
		if bytes.Equal(bytes.TrimSpace(intervalRaw), []byte("null")) || json.Unmarshal(intervalRaw, &minutes) != nil || minutes < 0 || minutes > 1440 {
			return fmt.Errorf("probe_interval_minutes must be an integer from 0 to 1440")
		}
		updates[auth.ProbeIntervalCredentialKey] = minutes
	}
	return nil
}
