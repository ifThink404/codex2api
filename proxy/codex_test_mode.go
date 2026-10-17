package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

type codexTestModeKey struct{}

// Only administrator test handlers install this request-local override.
func WithCodexTestMode(ctx context.Context, mode string) (context.Context, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "codex" {
		return ctx, fmt.Errorf("测试路径无效，请重新选择")
	}
	return context.WithValue(ctx, codexTestModeKey{}, mode), nil
}

func ValidateCodexTestMode(ctx context.Context, account *auth.Account) error {
	mode, _ := ctx.Value(codexTestModeKey{}).(string)
	if mode == "" || mode == "auto" {
		return nil
	}
	native := account != nil
	if account != nil {
		account.Mu().RLock()
		kind := strings.TrimSpace(account.UpstreamType)
		account.Mu().RUnlock()
		native = kind == "" || kind == "codex"
	}
	if !native || account.IsRelayStyle() {
		return fmt.Errorf("该账号不支持所选测试路径，请使用按账号配置")
	}
	return nil
}

func CodexTestModeLabel(ctx context.Context, account *auth.Account, models ...string) string {
	mode, _ := ctx.Value(codexTestModeKey{}).(string)
	if mode != "" && mode != "auto" {
		return mode
	}
	if account == nil || account.IsRelayStyle() {
		return "account"
	}
	return "codex"
}

// ExecuteCodexConnectionTest runs an account connection test after checking
// the explicit test mode (WithCodexTestMode); the mode never changes account
// settings.
func ExecuteCodexConnectionTest(ctx context.Context, account *auth.Account, payload []byte, proxyURL string) (*http.Response, error) {
	if err := ValidateCodexTestMode(ctx, account); err != nil {
		return nil, err
	}
	return ExecuteRequest(ctx, account, payload, "", proxyURL, "", nil, nil)
}
