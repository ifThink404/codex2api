package auth

import "testing"

func TestIsAPIKeyAccountClassifiesCredentialsNotRelayStyle(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{"nil", nil, false},
		{"responses", &Account{UpstreamType: UpstreamOpenAIResponses, BaseURL: "https://example.test", APIKey: "key"}, true},
		{"claude key", &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindAPIKey, AccessToken: "key"}, true},
		{"claude oauth", &Account{UpstreamType: UpstreamClaude, AccessToken: "at", RefreshToken: "rt"}, false},
		{"claude setup", &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindSetupToken, AccessToken: "sk-ant-oat01-test"}, false},
		{"grok key", &Account{UpstreamType: UpstreamGrok, APIKey: "key"}, true},
		{"grok oauth", &Account{UpstreamType: UpstreamGrok, AccessToken: "at", RefreshToken: "rt"}, false},
		{"grok sso", &Account{UpstreamType: UpstreamGrok, AccessToken: "sso"}, false},
		{"antigravity key", &Account{UpstreamType: UpstreamAntigravity, APIKey: "key"}, true},
		{"antigravity oauth", &Account{UpstreamType: UpstreamAntigravity, AccessToken: "at", RefreshToken: "rt"}, false},
		{"codex oauth", &Account{AccessToken: "at", RefreshToken: "rt"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.IsAPIKeyAccount(); got != tt.want {
				t.Fatalf("IsAPIKeyAccount() = %v, want %v", got, tt.want)
			}
			if tt.account != nil {
				tt.account.mu.RLock()
				defer tt.account.mu.RUnlock()
			}
			if got := tt.account.isAPIKeyAccountLocked(); got != tt.want {
				t.Fatalf("isAPIKeyAccountLocked() = %v, want %v", got, tt.want)
			}
		})
	}
}
