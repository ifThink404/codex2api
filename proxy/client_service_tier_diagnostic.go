package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

type clientServiceTierDiagnostic struct {
	State    string  `json:"state"`
	Value    *string `json:"value,omitempty"`
	Redacted bool    `json:"redacted,omitempty"`
}

func captureClientServiceTier(body []byte) *clientServiceTierDiagnostic {
	v := gjson.GetBytes(body, "service_tier")
	d := &clientServiceTierDiagnostic{State: "absent"}
	if !v.Exists() {
		return d
	}
	d.State = strings.ToLower(v.Type.String())
	if v.IsArray() {
		d.State = "array"
	} else if v.IsObject() {
		d.State = "object"
	}
	if v.Type == gjson.String {
		value := v.String()
		if len(value) <= 64 && (knownBillingTier(value) != "" || strings.TrimSpace(value) == "") {
			d.Value = &value
		} else {
			d.Redacted = true
		}
	}
	return d
}
