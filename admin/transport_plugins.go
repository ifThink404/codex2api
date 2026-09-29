package admin

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

// Transport plugin admin API (backend for the /plugins pages). Usage logs of a
// plugin are served by the existing usage-log endpoints with ?transport=<id>.

func (h *Handler) registerTransportPluginRoutes(api *gin.RouterGroup) {
	api.GET("/plugins", h.ListTransportPlugins)
	api.GET("/plugins/:plugin", h.GetTransportPlugin)
	api.PUT("/plugins/:plugin", h.UpdateTransportPlugin)
	api.PUT("/plugins/:plugin/accounts/:id", h.SetTransportPluginAccountOverride)
	api.GET("/plugins/:plugin/account-status", h.GetTransportPluginAccountStatus)
	api.GET("/plugins/:plugin/captures", h.ListTransportPluginCaptures)
	api.GET("/plugins/:plugin/captures/:captureId", h.GetTransportPluginCapture)
	api.GET("/plugins/:plugin/capture-stats", h.GetTransportPluginCaptureStats)
	api.GET("/plugins/:plugin/policy-blocks", h.GetTransportPluginPolicyBlocks)
	api.POST("/plugins/:plugin/captures/purge", h.PurgeTransportPluginCaptures)
}

type transportPluginAccountOverride struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
}

type transportPluginResponse struct {
	ID                    string                           `json:"id"`
	Meta                  plugins.Meta                     `json:"meta"`
	OverrideCredentialKey string                           `json:"override_credential_key"`
	State                 database.TransportPluginState    `json:"state"`
	Overrides             []transportPluginAccountOverride `json:"overrides"`
}

func (h *Handler) transportPluginResponse(p plugins.Plugin) transportPluginResponse {
	registry := plugins.Default()
	resp := transportPluginResponse{
		ID:                    p.ID(),
		Meta:                  p.Describe(),
		OverrideCredentialKey: plugins.OverrideCredentialKey(p),
		State:                 registry.State(p.ID()),
		Overrides:             []transportPluginAccountOverride{},
	}
	if h.store != nil {
		for _, account := range h.store.Accounts() {
			if enabled, ok := account.TransportPluginOverride(p.ID()); ok {
				account.Mu().RLock()
				name := account.Email
				account.Mu().RUnlock()
				resp.Overrides = append(resp.Overrides, transportPluginAccountOverride{AccountID: account.ID(), Name: name, Enabled: enabled})
			}
		}
	}
	return resp
}

func transportPluginFromParam(c *gin.Context) (plugins.Plugin, bool) {
	p, ok := plugins.Default().Get(strings.TrimSpace(c.Param("plugin")))
	if !ok {
		writeError(c, http.StatusNotFound, "传输插件不存在")
		return nil, false
	}
	return p, true
}

// ListTransportPlugins returns every compiled-in plugin with its active state.
func (h *Handler) ListTransportPlugins(c *gin.Context) {
	list := plugins.Default().Plugins()
	out := make([]transportPluginResponse, 0, len(list))
	for _, p := range list {
		out = append(out, h.transportPluginResponse(p))
	}
	written, dropped := plugins.Default().CaptureStats()
	c.JSON(http.StatusOK, gin.H{"plugins": out, "capture_written": written, "capture_dropped": dropped})
}

func (h *Handler) GetTransportPlugin(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.transportPluginResponse(p))
}

// updateTransportPluginRequest is a partial update: omitted fields keep their
// current value.
type updateTransportPluginRequest struct {
	Enabled           *bool           `json:"enabled"`
	GroupIDs          *[]int64        `json:"group_ids"`
	Config            json.RawMessage `json:"config"`
	CaptureEnabled    *bool           `json:"capture_enabled"`
	CaptureSampleRate *float64        `json:"capture_sample_rate"`
}

func (h *Handler) UpdateTransportPlugin(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	var req updateTransportPluginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	state := plugins.Default().State(p.ID())
	if req.Enabled != nil {
		state.Enabled = *req.Enabled
	}
	if req.GroupIDs != nil {
		state.GroupIDs = *req.GroupIDs
	}
	if len(req.Config) > 0 {
		state.Config = req.Config
	}
	if req.CaptureEnabled != nil {
		state.CaptureEnabled = *req.CaptureEnabled
	}
	if req.CaptureSampleRate != nil {
		state.CaptureSampleRate = *req.CaptureSampleRate
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := plugins.Default().Save(ctx, state); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, h.transportPluginResponse(p))
}

// SetTransportPluginAccountOverride sets ({"enabled":true|false}) or clears
// ({"enabled":null}) one account's override.
func (h *Handler) SetTransportPluginAccountOverride(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if h.store == nil || h.store.FindByID(accountID) == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.SetAccountTransportPluginOverride(ctx, accountID, plugins.OverrideCredentialKey(p), req.Enabled); err != nil {
		writeInternalError(c, err)
		return
	}
	h.store.ApplyAccountTransportPluginOverride(accountID, p.ID(), req.Enabled)
	c.JSON(http.StatusOK, gin.H{"account_id": accountID, "plugin": p.ID(), "enabled": req.Enabled})
}

// GetTransportPluginAccountStatus returns the plugin-scoped state (BPS
// cooldowns and their reasons) of the accounts in ?ids=1,2,3 (at most 200).
func (h *Handler) GetTransportPluginAccountStatus(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	var ids []int64
	for _, raw := range strings.Split(c.Query("ids"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(c, http.StatusBadRequest, "无效的账号 ID")
			return
		}
		ids = append(ids, id)
	}
	if len(ids) > 200 {
		writeError(c, http.StatusBadRequest, "一次最多查询 200 个账号")
		return
	}
	statuses := []proxy.BPSAccountStatus{}
	if p.ID() == proxy.BPSPluginID && len(ids) > 0 {
		statuses = proxy.BPSAccountStatuses(c.Request.Context(), h.cache, ids)
	}
	c.JSON(http.StatusOK, gin.H{"accounts": statuses})
}

func parseTransportPluginTime(c *gin.Context, name string) (time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return time.Time{}, true
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(c, http.StatusBadRequest, name+" 参数无效，需要 RFC3339 时间")
		return time.Time{}, false
	}
	return parsed, true
}

// ListTransportPluginCaptures filters by request_id, account_id, status,
// direction and start/end (RFC3339); bodies are omitted from list rows.
func (h *Handler) ListTransportPluginCaptures(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	filter := database.PluginCaptureFilter{
		Plugin:    p.ID(),
		RequestID: strings.TrimSpace(c.Query("request_id")),
		Direction: strings.TrimSpace(c.Query("direction")),
	}
	if filter.AccountID, ok = parseOpsErrorPositiveInt64(c, "account_id"); !ok {
		return
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		status, err := strconv.Atoi(raw)
		if err != nil || status < 0 {
			writeError(c, http.StatusBadRequest, "status 参数无效")
			return
		}
		filter.Status = &status
	}
	if filter.Start, ok = parseTransportPluginTime(c, "start"); !ok {
		return
	}
	if filter.End, ok = parseTransportPluginTime(c, "end"); !ok {
		return
	}
	filter.Page, _ = strconv.Atoi(c.Query("page"))
	filter.PageSize, _ = strconv.Atoi(c.Query("page_size"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	page, err := h.db.ListPluginCaptures(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h *Handler) GetTransportPluginCapture(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("captureId"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的抓包 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	capture, err := h.db.GetPluginCapture(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if capture == nil || capture.Plugin != p.ID() {
		writeError(c, http.StatusNotFound, "抓包记录不存在")
		return
	}
	c.JSON(http.StatusOK, capture)
}

// GetTransportPluginCaptureStats returns the plugin's stored capture rows,
// error rows, payload bytes and (PostgreSQL) the table's on-disk size.
func (h *Handler) GetTransportPluginCaptureStats(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	stats, err := h.db.PluginCaptureStats(c.Request.Context(), p.ID())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// purgeTransportPluginCapturesRequest: mode all, errors_only, or older_than
// with hours (1 to 720).
type purgeTransportPluginCapturesRequest struct {
	Mode  string `json:"mode"`
	Hours int    `json:"hours"`
}

// PurgeTransportPluginCaptures deletes the plugin's captures in batches, then
// vacuums the table on PostgreSQL after a large purge.
func (h *Handler) PurgeTransportPluginCaptures(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	var req purgeTransportPluginCapturesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	var cutoff time.Time
	switch req.Mode {
	case database.PluginCapturePurgeAll, database.PluginCapturePurgeErrorsOnly:
	case database.PluginCapturePurgeOlderThan:
		if req.Hours < 1 || req.Hours > 720 {
			writeError(c, http.StatusBadRequest, "hours 必须在 1 到 720 之间")
			return
		}
		cutoff = time.Now().Add(-time.Duration(req.Hours) * time.Hour)
	default:
		writeError(c, http.StatusBadRequest, "mode 必须是 all、errors_only 或 older_than")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	result, err := h.db.PurgePluginCapturesByMode(ctx, p.ID(), req.Mode, cutoff)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if result.Deleted >= database.PluginCaptureVacuumThreshold {
		if err := h.db.VacuumPluginCaptures(ctx); err != nil {
			log.Printf("[transport-plugin] capture vacuum after manual purge failed: %v", err)
		}
	}
	log.Printf("[transport-plugin] %s captures purged manually: mode=%s hours=%d deleted=%d", p.ID(), req.Mode, req.Hours, result.Deleted)
	c.JSON(http.StatusOK, result)
}

// bpsPolicyBlockView is one block event for the plugin page.
type bpsPolicyBlockView struct {
	database.BPSPolicyBlock
	Name           string     `json:"name"`
	ElapsedSeconds int64      `json:"elapsed_seconds"`
	Tiers          int        `json:"tiers,omitempty"`
	NextProbeAt    *time.Time `json:"next_probe_at,omitempty"`
}

type bpsPolicyBlockTotalsView struct {
	database.BPSPolicyBlockTotals
	Name string `json:"name"`
}

// GetTransportPluginPolicyBlocks returns the BPS usage-policy blocks: active
// ones (elapsed so far, tier, next probe), recent cleared ones (?limit=,
// default 100) with their durations, and per-account totals.
func (h *Handler) GetTransportPluginPolicyBlocks(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	out := gin.H{"active": []bpsPolicyBlockView{}, "history": []bpsPolicyBlockView{}, "totals": []bpsPolicyBlockTotalsView{}}
	if p.ID() != proxy.BPSPluginID {
		c.JSON(http.StatusOK, out)
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	now := time.Now()
	active, history, err := h.db.ListBPSPolicyBlocks(c.Request.Context(), limit)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	totals, err := h.db.BPSPolicyBlockTotalsAt(c.Request.Context(), now)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	names := map[int64]string{}
	name := func(id int64) string {
		if cached, ok := names[id]; ok {
			return cached
		}
		label := ""
		if h.store != nil {
			if account := h.store.FindByID(id); account != nil {
				account.Mu().RLock()
				label = account.Email
				account.Mu().RUnlock()
			}
		}
		names[id] = label
		return label
	}
	ids := make([]int64, 0, len(active))
	for _, block := range active {
		ids = append(ids, block.AccountID)
	}
	live := map[int64]proxy.BPSAccountStatus{}
	for _, status := range proxy.BPSAccountStatuses(c.Request.Context(), h.cache, ids) {
		live[status.AccountID] = status
	}
	activeViews := make([]bpsPolicyBlockView, 0, len(active))
	for _, block := range active {
		view := bpsPolicyBlockView{BPSPolicyBlock: block, Name: name(block.AccountID), ElapsedSeconds: int64(block.Elapsed(now) / time.Second)}
		if status, ok := live[block.AccountID]; ok {
			view.Tiers = status.PolicyTiers
			if status.PolicyTier > view.Tier {
				view.Tier = status.PolicyTier
			}
			if !status.NextProbeAt.IsZero() {
				next := status.NextProbeAt
				view.NextProbeAt = &next
			}
		}
		activeViews = append(activeViews, view)
	}
	historyViews := make([]bpsPolicyBlockView, 0, len(history))
	for _, block := range history {
		historyViews = append(historyViews, bpsPolicyBlockView{BPSPolicyBlock: block, Name: name(block.AccountID), ElapsedSeconds: int64(block.Elapsed(now) / time.Second)})
	}
	totalViews := make([]bpsPolicyBlockTotalsView, 0, len(totals))
	for _, total := range totals {
		totalViews = append(totalViews, bpsPolicyBlockTotalsView{BPSPolicyBlockTotals: total, Name: name(total.AccountID)})
	}
	out["active"], out["history"], out["totals"], out["now"] = activeViews, historyViews, totalViews, now.UTC()
	c.JSON(http.StatusOK, out)
}
