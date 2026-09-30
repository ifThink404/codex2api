package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func (h *Handler) UpdatePromptFilterBuiltinRule(c *gin.Context) {
	var request struct {
		Expected *promptfilter.BuiltinPatternOverride `json:"expected"`
		Rule     *promptfilter.BuiltinPatternOverride `json:"rule"` // null restores release defaults
	}
	if c.ShouldBindJSON(&request) != nil || request.Expected == nil {
		writeError(c, http.StatusBadRequest, "请提交规则内容和编辑前的规则快照")
		return
	}
	name := c.Param("name")
	var original *promptfilter.BuiltinPatternOverride
	for _, pattern := range promptfilter.BuiltinPatternConfigs() {
		if pattern.Name == name {
			fields := promptfilter.BuiltinPatternFields(pattern)
			original = &fields
			break
		}
	}
	if original == nil {
		writeError(c, http.StatusNotFound, "内置规则不存在")
		return
	}
	if request.Expected.Name != name || request.Rule != nil && request.Rule.Name != name {
		writeError(c, http.StatusBadRequest, "内置规则标识不能修改")
		return
	}
	if request.Rule != nil {
		if err := promptfilter.ValidateBuiltinPatternOverride(*request.Rule); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	persisted, err := h.db.GetSystemSettings(c.Request.Context())
	if err != nil || persisted == nil {
		writeError(c, http.StatusInternalServerError, "读取内置规则配置失败")
		return
	}
	raw := strings.TrimSpace(persisted.PromptFilterBuiltinOverrides)
	if raw == "" {
		raw = "[]"
	}
	overrides, err := promptfilter.ParseBuiltinPatternOverrides(raw)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "内置规则配置无效")
		return
	}
	current := *original
	for _, pattern := range promptfilter.EffectiveBuiltinPatternConfigs(overrides) {
		if pattern.Name == name {
			current = promptfilter.BuiltinPatternFields(pattern)
			break
		}
	}
	if !promptfilter.BuiltinPatternOverridesEqual(current, *request.Expected) {
		writeError(c, http.StatusConflict, "这条内置规则已被其他页面或实例修改，请刷新后重新编辑")
		return
	}
	next := make([]promptfilter.BuiltinPatternOverride, 0, len(overrides)+1)
	for _, rule := range overrides {
		if rule.Name != name {
			next = append(next, rule)
		}
	}
	if request.Rule != nil && !promptfilter.BuiltinPatternOverridesEqual(*request.Rule, *original) {
		next = append(next, *request.Rule)
	}
	replacement, _ := json.Marshal(next)
	swapped, err := h.db.CompareAndSwapPromptFilterBuiltinOverrides(c.Request.Context(), raw, string(replacement))
	if err != nil {
		writeError(c, http.StatusInternalServerError, "保存内置规则失败")
		return
	}
	if !swapped {
		writeError(c, http.StatusConflict, "内置规则配置已更新，请刷新后重试")
		return
	}
	cfg := h.store.GetPromptFilterConfig()
	cfg.BuiltinOverrides = next
	h.store.SetPromptFilterConfig(cfg)
	h.GetPromptFilterRules(c)
}
