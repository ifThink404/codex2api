package proxy

import (
	"context"
	"strconv"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type sessionOutboundEpochContextKey struct{}

type sessionOutboundEpoch struct {
	handler         *Handler
	key             string
	record          database.SessionContinuityRecord
	preview         bool
	temporary       bool // Executable request-local segment; no persistent root ownership.
	owner           string
	upstreamAccount string
	diagnostic      *sessionAccountFailoverDiagnostic
	preservedInput  []byte
}

func outboundEpochFromContext(ctx context.Context) *sessionOutboundEpoch {
	if ctx == nil {
		return nil
	}
	epoch, _ := ctx.Value(sessionOutboundEpochContextKey{}).(*sessionOutboundEpoch)
	return epoch
}

func (epoch *sessionOutboundEpoch) identityKey() string {
	if epoch == nil || !epoch.record.OutboundWindowReset {
		return ""
	}
	return codexIdentityDigest("codex-outbound-segment-v1", epoch.key, strconv.FormatUint(epoch.record.FailoverCount, 10))
}

func (handler *Handler) attachSessionOutboundEpoch(request *gin.Context, key string, record database.SessionContinuityRecord) {
	var epoch *sessionOutboundEpoch
	if key != "" && record.AccountID > 0 {
		usageRequestDiagnosticState(request).UpstreamRoute = &codexRouteDiagnostic{Mode: normalizedCodexRoute(record.UpstreamMode), AccountID: record.AccountID, Generation: record.FailoverCount}
		epoch = &sessionOutboundEpoch{handler: handler, key: key, record: record, owner: responseCacheOwnerForRequest(request, requestAPIKeyID(request))}
		if handler.store != nil {
			if account := handler.store.FindByID(record.AccountID); account != nil {
				epoch.upstreamAccount = account.EffectiveAccountID()
			}
		}
		if record.LossyContextRestart {
			state := usageRequestDiagnosticState(request)
			if state.AccountFailover == nil {
				state.AccountFailover = &sessionAccountFailoverDiagnostic{UpstreamMode: normalizedCodexRoute(record.UpstreamMode), Result: "restored", Phase: "after_switch", Reason: record.LastFailoverReason, PreviousAccountID: record.PreviousAccountID, AccountID: record.AccountID, Generation: record.FailoverCount}
			}
			epoch.diagnostic = state.AccountFailover
		}
	}
	if record.PreserveRestartInput {
		if raw, exists := request.Get(preservedInputSnapshotKey); exists {
			if body, ok := raw.([]byte); ok {
				epoch.preservedInput = []byte(gjson.GetBytes(body, "input").Raw)
			}
		}
		if plan, _ := request.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan); len(epoch.preservedInput) == 0 && plan != nil && plan.PreserveInput {
			epoch.preservedInput = []byte(gjson.GetBytes(plan.Body, "input").Raw)
		}
	}
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), sessionOutboundEpochContextKey{}, epoch))
}

func validateSessionOutboundEpoch(ctx context.Context, account *auth.Account) error {
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil {
		return nil
	}
	if account == nil || account.ID() != epoch.record.AccountID {
		return codexAccountIdentityError("请求账号与当前迁移段不一致，已停止发送正文。")
	}
	if epoch.upstreamAccount != "" && account.EffectiveAccountID() != epoch.upstreamAccount {
		return codexAccountIdentityError("请求的上游账号身份已变化，已停止发送正文。")
	}
	if epoch.preview || epoch.temporary {
		return nil
	}
	entry, found, err := epoch.handler.readSessionContinuity(ctx, epoch.key)
	if err != nil || !found || entry.Record.AccountID != epoch.record.AccountID || entry.Record.FailoverCount != epoch.record.FailoverCount || normalizedCodexRoute(entry.Record.UpstreamMode) != normalizedCodexRoute(epoch.record.UpstreamMode) || entry.Record.OutboundWindowReset != epoch.record.OutboundWindowReset {
		return codexAccountIdentityError("会话账号迁移段已变化或暂时无法核实，请重新发起请求。")
	}
	return nil
}
