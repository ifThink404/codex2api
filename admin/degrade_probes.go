package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// Degradation ("降智") judge probes of the BPS plugin: list, sample review
// and manual / batch probes (queued; at most degrade_probe_max_concurrent
// run at a time).

type degradeProbeView struct {
	database.DegradeProbe
	Name string `json:"name"`
}

func (h *Handler) degradeProbePlugin(c *gin.Context) bool {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return false
	}
	if p.ID() != proxy.BPSPluginID {
		writeError(c, http.StatusNotFound, "该插件没有降智检测")
		return false
	}
	return true
}

func (h *Handler) accountLabel(id int64) string {
	account := h.findAccount(id)
	if account == nil {
		return ""
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return account.Email
}

// ListDegradeProbes returns probes newest first (?account_id=&route=&verdict=&limit=).
func (h *Handler) ListDegradeProbes(c *gin.Context) {
	if !h.degradeProbePlugin(c) {
		return
	}
	filter := database.DegradeProbeFilter{Route: strings.TrimSpace(c.Query("route")), Verdict: strings.TrimSpace(c.Query("verdict"))}
	if raw := strings.TrimSpace(c.Query("account_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(c, http.StatusBadRequest, "account_id 参数无效")
			return
		}
		filter.AccountID = id
	}
	filter.Limit, _ = strconv.Atoi(c.DefaultQuery("limit", "100"))
	probes, err := h.db.ListDegradeProbes(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	views := make([]degradeProbeView, 0, len(probes))
	for _, probe := range probes {
		views = append(views, degradeProbeView{DegradeProbe: probe, Name: h.accountLabel(probe.AccountID)})
	}
	threshold, model := proxy.DegradeJudgeSettings()
	c.JSON(http.StatusOK, gin.H{"probes": views, "threshold": threshold, "model": model})
}

// GetDegradeProbe returns one probe with its HTML sample.
func (h *Handler) GetDegradeProbe(c *gin.Context) {
	if !h.degradeProbePlugin(c) {
		return
	}
	id, err := strconv.ParseInt(c.Param("probeId"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的检测 ID")
		return
	}
	probe, err := h.db.GetDegradeProbe(c.Request.Context(), id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if probe == nil {
		writeError(c, http.StatusNotFound, "检测记录不存在")
		return
	}
	c.JSON(http.StatusOK, degradeProbeView{DegradeProbe: *probe, Name: h.accountLabel(probe.AccountID)})
}

// StartDegradeProbes queues pelican probes of route for the accounts
// ({"account_ids":[...],"route":"bps|native"}); already queued or running
// ones and accounts that cannot be probed on that route are reported.
func (h *Handler) StartDegradeProbes(c *gin.Context) {
	if !h.degradeProbePlugin(c) {
		return
	}
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
		Route      string  `json:"route"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.AccountIDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供 account_ids 与 route")
		return
	}
	if len(req.AccountIDs) > 500 {
		writeError(c, http.StatusBadRequest, "一次最多检测 500 个账号")
		return
	}
	route := strings.TrimSpace(req.Route)
	if route != proxy.RouteBPS && route != proxy.RouteNative {
		writeError(c, http.StatusBadRequest, "route 必须是 bps 或 native")
		return
	}
	if h.authCacheProxy == nil {
		writeError(c, http.StatusServiceUnavailable, "降智检测未就绪")
		return
	}
	type skipped struct {
		AccountID int64  `json:"account_id"`
		Reason    string `json:"reason"`
	}
	queued, skips := 0, []skipped{}
	for _, id := range uniqueAccountIDs(req.AccountIDs) {
		if err := proxy.CheckDegradeRoute(h.findAccount(id), route); err != nil {
			skips = append(skips, skipped{AccountID: id, Reason: err.Error()})
			continue
		}
		if !h.authCacheProxy.EnqueueDegradeProbe(id, route, proxy.DegradeTriggerManual) {
			skips = append(skips, skipped{AccountID: id, Reason: "已在检测队列中"})
			continue
		}
		queued++
	}
	c.JSON(http.StatusAccepted, gin.H{"queued": queued, "skipped": skips})
}
