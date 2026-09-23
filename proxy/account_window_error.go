package proxy

import (
	"fmt"
	"strconv"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// accountWindowUnavailableAPIError is shared by HTTP and WebSocket selection
// failures. A full but otherwise healthy account must not be hidden by an
// unrelated quota-exhausted candidate elsewhere in the eligible pool.
func (h *Handler) accountWindowUnavailableAPIError(c *gin.Context, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy, sessionKey string) *api.APIError {
	if h.store.HasSessionCapacityExhaustionWithDispatch(apiKeyID, exclude, filter, policy, sessionKey, time.Now()) {
		return api.NewAPIError(api.ErrCodeAccountSessionCapacity, accountSessionCapacityExceededMessage, api.ErrorTypeInvalidRequest)
	}
	recovery, limited := h.store.UsageLimitRecoveryWithDispatch(apiKeyID, exclude, filter, policy)
	if !limited {
		return nil
	}
	message := "上游 Codex 账号用量额度已达上限，暂未获取到恢复时间，请稍后重试。这不是用户新建窗口数量或创建冷却限制"
	if wait := time.Until(recovery); wait > 0 {
		seconds := int((wait + time.Second - 1) / time.Second)
		message = fmt.Sprintf("上游 Codex 账号用量额度已达上限，预计最早于 %s 恢复（约 %d 分 %d 秒后）。这不是用户新建窗口数量或创建冷却限制", recovery.UTC().Format("2006-01-02 15:04:05 UTC"), seconds/60, seconds%60)
		if c != nil && !c.Writer.Written() {
			c.Header("Retry-After", strconv.Itoa(seconds))
		}
	}
	return api.NewAPIError(api.ErrCodeRateLimitReached, message, api.ErrorTypeRateLimit)
}
