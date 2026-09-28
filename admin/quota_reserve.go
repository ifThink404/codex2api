package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) GetQuotaReserveEligibility(c *gin.Context) {
	id, err := strconv.ParseInt(c.Query("api_key_id"), 10, 64)
	model, mode := strings.TrimSpace(c.Query("model")), strings.TrimSpace(c.Query("mode"))
	if err != nil || id <= 0 || model == "" || len(model) > 128 || (mode != "" && mode != "bps" && mode != "native") {
		writeError(c, http.StatusBadRequest, "请填写有效的 Key ID、模型和路径")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	key, err := h.db.GetAPIKeyByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && key == nil {
		writeError(c, http.StatusNotFound, "API Key 不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	allowed, available, reason := proxy.QuotaReserveEligibility(h.store, key, model, mode)
	c.JSON(http.StatusOK, gin.H{"version": 1, "account_ids": allowed, "available_account_ids": available, "reason": reason, "observed_at": time.Now().UTC().Format(time.RFC3339)})
}
