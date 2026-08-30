package auth

import "testing"

func TestNormalizeClaudeSecurityConfigUsesSafeDefaults(t *testing.T) {
	cfg := NormalizeClaudeSecurityConfig(ClaudeSecurityConfig{})
	if cfg.MaxOutputTokens != 8192 || cfg.MaxToolCount != 16 || cfg.MaxToolSchemaBytes != 131072 {
		t.Fatalf("secure defaults = %+v", cfg)
	}
	if len(cfg.AllowedBetaHeaders) != 0 {
		t.Fatalf("empty beta allowlist should stay empty: %v", cfg.AllowedBetaHeaders)
	}
}

func TestNormalizeClaudeSecurityConfigCanonicalizesBetaAllowlistAndBounds(t *testing.T) {
	cfg := NormalizeClaudeSecurityConfig(ClaudeSecurityConfig{
		AllowedBetaHeaders: []string{" Foo-Bar ", "foo-bar", "bad value", "oauth-2025-04-20"},
		MaxOutputTokens:    999999,
		MaxToolCount:       999,
		MaxToolSchemaBytes: 99999999,
	})
	if len(cfg.AllowedBetaHeaders) != 2 || cfg.AllowedBetaHeaders[0] != "foo-bar" || cfg.AllowedBetaHeaders[1] != "oauth-2025-04-20" {
		t.Fatalf("normalized beta allowlist = %v", cfg.AllowedBetaHeaders)
	}
	if cfg.MaxOutputTokens != 131072 || cfg.MaxToolCount != 64 || cfg.MaxToolSchemaBytes != 1048576 {
		t.Fatalf("bounded limits = %+v", cfg)
	}
}

func TestParseClaudeConfigKeepsLegacyFieldsAndSecurityDefaults(t *testing.T) {
	cfg := ParseClaudeConfig(`{"fingerprint_mode":"force","default_timezone":"Asia/Shanghai","session_window_limit":4,"allow_service_tier":true,"allowed_beta_headers":["beta-x"]}`)
	if cfg.FingerprintMode != ClaudeFingerprintModeForce || cfg.DefaultTimezone != "Asia/Shanghai" || cfg.SessionWindowLimit != 4 {
		t.Fatalf("legacy Claude config fields changed: %+v", cfg)
	}
	security := cfg.SecurityConfig()
	if !security.AllowServiceTier || len(security.AllowedBetaHeaders) != 1 || security.MaxOutputTokens != 8192 {
		t.Fatalf("security config parse = %+v", security)
	}
}
