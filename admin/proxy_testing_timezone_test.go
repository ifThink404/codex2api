package admin

import (
	"strings"
	"testing"
)

// TestParseIPAPIProbeBodyCapturesTimezone 覆盖 ip-api 返回体里 timezone 字段的解析：
// 正常 IANA 时区名要透传，缺失字段和非法值都要落空。
func TestParseIPAPIProbeBodyCapturesTimezone(t *testing.T) {
	body := []byte(`{"status":"success","query":"1.2.3.4","country":"United States","regionName":"California","city":"Los Angeles","isp":"X","timezone":"America/Los_Angeles"}`)
	result := parseIPAPIProbeBody(body, 42)
	if !result.Success {
		t.Fatalf("parseIPAPIProbeBody success = %v, want true", result.Success)
	}
	if result.Timezone != "America/Los_Angeles" {
		t.Fatalf("Timezone = %q, want %q", result.Timezone, "America/Los_Angeles")
	}
}

func TestParseIPAPIProbeBodyWithoutTimezoneFieldIsEmpty(t *testing.T) {
	body := []byte(`{"status":"success","query":"1.2.3.4","country":"United States","regionName":"California","city":"Los Angeles","isp":"X"}`)
	result := parseIPAPIProbeBody(body, 42)
	if !result.Success {
		t.Fatalf("parseIPAPIProbeBody success = %v, want true", result.Success)
	}
	if result.Timezone != "" {
		t.Fatalf("Timezone = %q, want empty", result.Timezone)
	}
}

func TestParseIPAPIProbeBodyRejectsGarbageTimezone(t *testing.T) {
	longGarbage := "Not/A/Real/Timezone/" + strings.Repeat("x", 60)
	cases := []struct {
		name string
		raw  string
	}{
		{"contains a space", "not/a tz"},
		{"too long", longGarbage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"status":"success","query":"1.2.3.4","country":"United States","regionName":"California","city":"Los Angeles","isp":"X","timezone":"` + tc.raw + `"}`)
			result := parseIPAPIProbeBody(body, 42)
			if !result.Success {
				t.Fatalf("parseIPAPIProbeBody success = %v, want true", result.Success)
			}
			if result.Timezone != "" {
				t.Fatalf("Timezone = %q, want empty for raw %q", result.Timezone, tc.raw)
			}
		})
	}
}

func TestValidProxyProbeTimezone(t *testing.T) {
	if got := validProxyProbeTimezone("America/Los_Angeles"); got != "America/Los_Angeles" {
		t.Fatalf("validProxyProbeTimezone(valid) = %q, want unchanged", got)
	}
	if got := validProxyProbeTimezone(""); got != "" {
		t.Fatalf("validProxyProbeTimezone(empty) = %q, want empty", got)
	}
	if got := validProxyProbeTimezone("not/a tz"); got != "" {
		t.Fatalf("validProxyProbeTimezone(garbage with space) = %q, want empty", got)
	}
	if got := validProxyProbeTimezone(strings.Repeat("a", proxyProbeTimezoneMaxLen+1)); got != "" {
		t.Fatalf("validProxyProbeTimezone(too long) = %q, want empty", got)
	}
}
