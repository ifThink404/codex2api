package proxy

import (
	"context"
	"fmt"
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
	if mode != "auto" && mode != "codex" && mode != "bps" {
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
		kind, authMode := strings.TrimSpace(account.UpstreamType), account.CodexAuthMode
		account.Mu().RUnlock()
		native = (kind == "" || kind == "codex") && !(mode == "bps" && strings.EqualFold(authMode, auth.CodexAuthModeAgentIdentity))
	}
	if !native || account.IsRelayStyle() || mode == "bps" && account.IsCodexAgentIdentity() {
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
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	if bpsServesAccount(account, model) && !account.CodexRouteAllows("native", model, false, true) {
		return "bps"
	}
	return "codex"
}
