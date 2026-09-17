package admin

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type sessionLockResponse struct {
	ID              int64  `json:"id"`
	SessionIDPrefix string `json:"session_id_prefix"`
	APIKeyID        int64  `json:"api_key_id"`
	AccountID       int64  `json:"account_id"`
	AccountName     string `json:"account_name"`
	ErrorMessage    string `json:"error_message"`
	Threshold       int    `json:"threshold"`
	Source          string `json:"source"`
	LockedAt        string `json:"locked_at"`
}

// ListSessionLocks 列出自动锁定的会话（最新在前）。
func (h *Handler) ListSessionLocks(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	limit, _ := strconv.Atoi(c.Query("limit"))
	locks, err := h.db.ListSessionAutoLocks(ctx, limit)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	out := make([]sessionLockResponse, 0, len(locks))
	for _, lock := range locks {
		name := ""
		if h.store != nil && lock.AccountID > 0 {
			if account := h.store.FindByID(lock.AccountID); account != nil {
				name = account.Email
			}
		}
		out = append(out, sessionLockResponse{
			ID: lock.ID, SessionIDPrefix: lock.SessionIDPrefix, APIKeyID: lock.APIKeyID, AccountID: lock.AccountID, AccountName: name,
			ErrorMessage: lock.ErrorMessage, Threshold: lock.Threshold, Source: lock.Source, LockedAt: lock.LockedAt.UTC().Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"locks": out, "total": len(out)})
}

// DeleteSessionLock 管理员解锁：删 DB 行并从进程内存移除。
func (h *Handler) DeleteSessionLock(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的锁 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	removed, err := h.db.DeleteSessionAutoLock(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if removed == nil {
		writeError(c, http.StatusNotFound, "锁不存在或已解除")
		return
	}
	if h.authCacheProxy != nil {
		h.authCacheProxy.UnlockSessionAutoLock(removed.SessionKey)
	}
	c.JSON(http.StatusOK, gin.H{"message": "unlocked"})
}
