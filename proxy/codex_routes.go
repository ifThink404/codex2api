package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type codexRouteRequestKey struct{}
type codexRouteRequest struct {
	Model     string
	Auxiliary bool
}
type codexRouteFloorKey struct{}

// Local diagnostics only; never embedded in the request body or headers.
type codexRouteDiagnostic struct {
	Mode       string `json:"mode"`
	AccountID  int64  `json:"account_id"`
	Generation uint64 `json:"generation"`
}

func normalizedCodexRoute(mode string) string {
	if mode == "" {
		return "native"
	}
	return mode
}

func codexRouteRequestInfo(ctx context.Context, models ...string) codexRouteRequest {
	if ctx == nil {
		ctx = context.Background()
	}
	info, _ := ctx.Value(codexRouteRequestKey{}).(codexRouteRequest)
	if len(models) > 0 && models[0] != "" {
		info.Model = models[0]
	}
	info.Auxiliary = info.Auxiliary || backgroundAccountMatchFromContext(ctx) != nil
	return info
}

// Applied after passive model authorization, so auxiliary requests can inherit
// the parent's route without being constrained by the main-turn model lists.
func codexRouteAccountFilter(c *gin.Context, next auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		ctx := c.Request.Context()
		info := codexRouteRequestInfo(ctx)
		prior, _ := ctx.Value(codexRouteFloorKey{}).(string)
		if epoch := outboundEpochFromContext(ctx); epoch != nil && epoch.record.UpstreamMode == "bps" {
			prior = "bps"
		}
		if account == nil {
			return false
		}
		if account.IsRelayStyle() {
			return prior != "bps" && (next == nil || next(account))
		}
		if selectCodexRoute(account, info.Model, prior, info.Auxiliary) == "" {
			selectionTraceForRequest(c).Reject("upstream_route_unavailable")
			return false
		}
		return next == nil || next(account)
	}
}

// Direct WS callers must obey the same route boundary as the HTTP dispatcher.
func ValidateCodexNativeRoute(ctx context.Context, account *auth.Account, body []byte) error {
	mode, err := codexRequestRouteMode(ctx, account, gjson.GetBytes(body, "model").String())
	if err != nil {
		return err
	}
	if mode != "native" {
		return codexRouteUnavailable(mode)
	}
	return nil
}

func selectCodexRoute(account *auth.Account, model, prior string, auxiliary bool) string {
	prior = normalizedCodexRoute(prior)
	if prior == "bps" {
		if account.CodexRouteAllows("bps", model, auxiliary) {
			return "bps"
		}
		return ""
	}
	if prior != "native" {
		return ""
	}
	if account.CodexRouteAllows("native", model, auxiliary) {
		return "native"
	}
	if account.CodexRouteAllows("bps", model, auxiliary) {
		return "bps"
	}
	return ""
}

func codexRouteUnavailable(mode string) error {
	message := "当前账号的请求路径已关闭或不支持所选模型，请启用禁用换号功能并配置可用路径，或新开对话。"
	if mode == "bps" {
		message = "当前会话已使用 BPS，不能转回 Codex；没有可用的 BPS 路径或所选模型未配置，请恢复 BPS、配置其他 BPS 账号或新开对话。"
	}
	return &Error{Code: "codex_upstream_route_unavailable", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

func codexRequestRouteMode(ctx context.Context, account *auth.Account, models ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if mode, _ := ctx.Value(codexTestModeKey{}).(string); mode != "" && mode != "auto" {
		if err := ValidateCodexTestMode(ctx, account); err != nil {
			return "", err
		}
		if mode == "bps" {
			return "bps", nil
		}
		return "native", nil
	}
	if account == nil || account.IsRelayStyle() {
		return "native", nil
	}
	info := codexRouteRequestInfo(ctx, models...)
	prior := ""
	if epoch := outboundEpochFromContext(ctx); epoch != nil && epoch.record.AccountID > 0 {
		prior = normalizedCodexRoute(epoch.record.UpstreamMode)
	} else if s, _ := ctx.Value(protocolIdentityKey{}).(*responseIdentitySession); s != nil && s.root != "" {
		entry, found, err := s.handler.readSessionContinuity(ctx, s.rootKey)
		if err != nil {
			return "", codexAccountIdentityError("无法确认会话的上游请求模式，请稍后重试。")
		}
		if found {
			prior = normalizedCodexRoute(entry.Record.UpstreamMode)
		}
	}
	floor, _ := ctx.Value(codexRouteFloorKey{}).(string)
	if floor == "bps" && prior == "native" {
		return "", codexRouteUnavailable("bps")
	}
	if prior != "" {
		// Existing roots migrate only through the serialized failover transaction.
		if account.CodexRouteAllows(prior, info.Model, info.Auxiliary) {
			return prior, nil
		}
		return "", codexRouteUnavailable(prior)
	}
	mode := selectCodexRoute(account, info.Model, floor, info.Auxiliary)
	if mode == "" {
		return "", codexRouteUnavailable(floor)
	}
	return mode, nil
}

func codexRouteFailureForRequest(ctx context.Context, account *auth.Account) string {
	if account == nil || account.IsRelayStyle() {
		return ""
	}
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || epoch.record.AccountID != account.ID() {
		return ""
	}
	info := codexRouteRequestInfo(ctx)
	mode := normalizedCodexRoute(epoch.record.UpstreamMode)
	if floor, _ := ctx.Value(codexRouteFloorKey{}).(string); floor == "bps" && mode != "bps" {
		return "upstream_route_requires_bps"
	}
	if !account.CodexRouteAllows(mode, "", true) {
		return "upstream_route_disabled"
	}
	if !account.CodexRouteAllows(mode, info.Model, info.Auxiliary) {
		return "upstream_route_model_unavailable"
	}
	return ""
}

func codexRouteIsAuxiliary(body []byte) bool {
	meta := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
	kind := strings.ToLower(meta.Get("request_kind").String())
	return kind == "compaction" || requestBodyHasCompactionTrigger(body)
}
