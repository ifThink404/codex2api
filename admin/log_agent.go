package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/logagent"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 日志分析 Agent 的管理端：配置、按需分析、分析记录。logagent 包只认 LLM / Source
// 两个接口；这里提供号池 LLM 实现与内置的 usage_logs / ops_errors 两个 Source。
// 插件通过 logagent.Register 注册自己的 Source，无需改动本文件。

const (
	logAgentInternalReasonPrefix = "log_agent:"
	logAgentFetchTimeout         = 15 * time.Second
	logAgentPurgeInterval        = time.Hour
	logAgentMaxRefs              = 50
	logAgentMaxFocusRunes        = 500
)

// logAgentState 挂在 Handler 上，惰性初始化，测试可替换 newLLM。
type logAgentState struct {
	once      sync.Once
	builtins  *logagent.Registry
	lastPurge atomic.Int64
	// newLLM 为 nil 时使用号池实现。
	newLLM func(h *Handler, key *database.APIKeyRow, source string, timeout time.Duration) logagent.LLM
}

func (h *Handler) registerLogAgentRoutes(api *gin.RouterGroup) {
	api.GET("/log-agent/config", h.GetLogAgentConfig)
	api.PUT("/log-agent/config", h.UpdateLogAgentConfig)
	api.POST("/log-agent/analyze", h.AnalyzeLogAgent)
	api.GET("/log-agent/runs", h.ListLogAgentRuns)
	api.GET("/log-agent/runs/:id", h.GetLogAgentRun)
}

func (h *Handler) logAgentBuiltins() *logagent.Registry {
	h.logAgent.once.Do(func() {
		registry := logagent.NewRegistry()
		_ = registry.Register(&usageLogAgentSource{db: h.db, name: logAgentSourceUsageLogs})
		_ = registry.Register(&usageLogAgentSource{db: h.db, name: logAgentSourceOpsErrors, errorOnly: true})
		h.logAgent.builtins = registry
	})
	return h.logAgent.builtins
}

// lookupLogAgentSource 先查内置 Source，再查插件注册的 logagent.Default。
func (h *Handler) lookupLogAgentSource(name string) (logagent.Source, bool) {
	if src, ok := h.logAgentBuiltins().Lookup(name); ok {
		return src, true
	}
	return logagent.Default.Lookup(name)
}

func (h *Handler) logAgentSourceNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, name := range append(h.logAgentBuiltins().Names(), logagent.Default.Names()...) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (h *Handler) GetLogAgentConfig(c *gin.Context) {
	ctx := c.Request.Context()
	cfg, err := h.db.LoadLogAgentConfig(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"config":       cfg,
		"gateway_keys": gatewayAPIKeyOptions(keys, time.Now()),
		"sources":      h.logAgentSourceNames(),
		"limits": gin.H{
			"min_max_input_bytes": logagent.MinMaxInputBytes,
			"max_max_input_bytes": logagent.MaxMaxInputBytes,
			"max_max_records":     logagent.MaxMaxRecords,
			"min_timeout_seconds": database.MinLogAgentTimeoutSeconds,
			"max_timeout_seconds": database.MaxLogAgentTimeoutSeconds,
			"max_retention_days":  database.MaxLogAgentRetentionDays,
		},
	})
}

func (h *Handler) UpdateLogAgentConfig(c *gin.Context) {
	var cfg database.LogAgentConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		writeError(c, http.StatusBadRequest, "日志分析配置无效")
		return
	}
	if cfg.APIKeyID > 0 {
		if _, err := h.db.GetAPIKeyByID(c.Request.Context(), cfg.APIKeyID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(c, http.StatusBadRequest, "选择的网关 API Key 不存在")
				return
			}
			writeInternalError(c, err)
			return
		}
	}
	saved, err := h.db.SaveLogAgentConfig(c.Request.Context(), cfg)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": saved})
}

// logAgentAnalyzeRequest 是分析请求：Refs 与 Filters 的含义由 Source 决定。
type logAgentAnalyzeRequest struct {
	Source   string            `json:"source"`
	Refs     []string          `json:"refs"`
	Filters  map[string]string `json:"filters"`
	Start    string            `json:"start"`
	End      string            `json:"end"`
	Focus    string            `json:"focus"`
	Language string            `json:"language"`
}

// logAgentSubject 是持久化到 log_agent_runs.subject 的分析对象描述。
type logAgentSubject struct {
	Refs     []string          `json:"refs,omitempty"`
	Filters  map[string]string `json:"filters,omitempty"`
	Start    *time.Time        `json:"start,omitempty"`
	End      *time.Time        `json:"end,omitempty"`
	Focus    string            `json:"focus,omitempty"`
	Language string            `json:"language,omitempty"`
}

func (h *Handler) AnalyzeLogAgent(c *gin.Context) {
	var request logAgentAnalyzeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "分析参数无效")
		return
	}
	ctx := c.Request.Context()
	cfg, err := h.db.LoadLogAgentConfig(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if !cfg.Enabled {
		writeError(c, http.StatusConflict, "日志分析 Agent 未启用，请先在系统设置中开启")
		return
	}
	if cfg.Model == "" {
		writeError(c, http.StatusBadRequest, "请先在系统设置中选择日志分析模型")
		return
	}
	request.Source = strings.TrimSpace(request.Source)
	src, ok := h.lookupLogAgentSource(request.Source)
	if !ok {
		writeError(c, http.StatusNotFound, "未知的日志来源")
		return
	}
	query, subject, err := buildLogAgentQuery(request, cfg.MaxRecords)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	key, err := h.resolveInternalGatewayAPIKey(ctx, cfg.APIKeyID)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	fetchCtx, cancelFetch := context.WithTimeout(ctx, logAgentFetchTimeout)
	records, err := src.Fetch(fetchCtx, query)
	cancelFetch()
	if err != nil {
		writeError(c, http.StatusBadRequest, "读取日志失败: "+err.Error())
		return
	}
	if len(records) == 0 {
		writeError(c, http.StatusUnprocessableEntity, "没有找到可分析的记录")
		return
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	llm := h.newLogAgentLLM(key, src.Name(), timeout)
	analyzeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, analyzeErr := logagent.Analyze(analyzeCtx, llm, logagent.Request{
		Source:   src.Name(),
		Model:    cfg.Model,
		Records:  records,
		Limits:   logagent.Limits{MaxInputBytes: cfg.MaxInputBytes, MaxRecords: cfg.MaxRecords},
		Focus:    subject.Focus,
		Language: subject.Language,
	})

	subjectJSON, _ := json.Marshal(subject)
	run := &database.LogAgentRun{
		Source:      src.Name(),
		Subject:     subjectJSON,
		Model:       cfg.Model,
		APIKeyID:    cfg.APIKeyID,
		RecordCount: len(records),
		Findings:    json.RawMessage(`{}`),
	}
	if result != nil {
		if analyzeErr == nil {
			run.Findings, _ = json.Marshal(result.Findings)
		}
		run.ContextStats, _ = json.Marshal(result.Context)
		run.InputTokens, run.OutputTokens, run.TotalTokens = result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens
		run.DurationMs = result.DurationMs
	}
	switch {
	case analyzeErr != nil:
		run.Status = database.LogAgentRunStatusFailed
		// Analyze 给模型错误加了包前缀，管理台只展示内层原因。
		message := analyzeErr.Error()
		if inner := errors.Unwrap(analyzeErr); inner != nil {
			message = inner.Error()
		}
		run.ErrorMessage = logagent.MaskText(message)
	case result.ParseError != "":
		run.Status = database.LogAgentRunStatusFallback
		run.ErrorMessage = result.ParseError
	default:
		run.Status = database.LogAgentRunStatusSucceeded
	}
	// 请求可能因客户端断开而取消；分析已花掉额度，记录仍要落库。
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelPersist()
	if err := h.db.InsertLogAgentRun(persistCtx, run); err != nil {
		writeInternalError(c, err)
		return
	}
	h.maybePurgeLogAgentRuns(persistCtx, cfg.RetentionDays)
	if analyzeErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": run.ErrorMessage, "run": run})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": run})
}

func buildLogAgentQuery(request logAgentAnalyzeRequest, limit int) (logagent.Query, logAgentSubject, error) {
	var subject logAgentSubject
	query := logagent.Query{Limit: limit}
	seen := map[string]bool{}
	for _, ref := range request.Refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		if len(ref) > 128 {
			return query, subject, errors.New("记录引用过长")
		}
		if len(query.Refs) >= logAgentMaxRefs {
			return query, subject, fmt.Errorf("一次最多分析 %d 条引用", logAgentMaxRefs)
		}
		seen[ref] = true
		query.Refs = append(query.Refs, ref)
	}
	if len(request.Filters) > 0 {
		query.Filters = make(map[string]string, len(request.Filters))
		for key, value := range request.Filters {
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			if len(key) > 64 || len(value) > 256 {
				return query, subject, errors.New("筛选条件过长")
			}
			query.Filters[key] = value
		}
	}
	start, end := strings.TrimSpace(request.Start), strings.TrimSpace(request.End)
	if start != "" || end != "" {
		if start == "" || end == "" {
			return query, subject, errors.New("start/end 参数需要同时提供")
		}
		parsedStart, e1 := time.Parse(time.RFC3339, start)
		parsedEnd, e2 := time.Parse(time.RFC3339, end)
		if e1 != nil || e2 != nil || !parsedEnd.After(parsedStart) {
			return query, subject, errors.New("start/end 参数格式错误，需要 RFC3339 格式")
		}
		query.Start, query.End = parsedStart, parsedEnd
		subject.Start, subject.End = &parsedStart, &parsedEnd
	}
	subject.Refs, subject.Filters = query.Refs, query.Filters
	subject.Focus = clampRunesText(strings.TrimSpace(request.Focus), logAgentMaxFocusRunes)
	subject.Language = strings.TrimSpace(request.Language)
	if len(subject.Language) > 16 {
		subject.Language = ""
	}
	return query, subject, nil
}

func clampRunesText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// maybePurgeLogAgentRuns 每小时至多清理一次过期分析记录；失败不影响本次分析结果。
func (h *Handler) maybePurgeLogAgentRuns(ctx context.Context, retentionDays int) {
	now := time.Now()
	last := h.logAgent.lastPurge.Load()
	if now.UnixNano()-last < int64(logAgentPurgeInterval) || !h.logAgent.lastPurge.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	if retentionDays <= 0 {
		retentionDays = database.DefaultLogAgentRetentionDays
	}
	_, _ = h.db.PurgeLogAgentRuns(ctx, now.Add(-time.Duration(retentionDays)*24*time.Hour))
}

func (h *Handler) ListLogAgentRuns(c *gin.Context) {
	filter := database.LogAgentRunFilter{Source: strings.TrimSpace(c.Query("source"))}
	if raw := strings.TrimSpace(c.Query("before_id")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			writeError(c, http.StatusBadRequest, "before_id 参数无效")
			return
		}
		filter.BeforeID = value
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 || value > 200 {
			writeError(c, http.StatusBadRequest, "limit 参数无效")
			return
		}
		filter.Limit = value
	}
	runs, err := h.db.ListLogAgentRuns(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

func (h *Handler) GetLogAgentRun(c *gin.Context) {
	id, err := parsePositiveInt64Param(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "分析记录 ID 无效")
		return
	}
	run, err := h.db.GetLogAgentRun(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "分析记录不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": run})
}

func (h *Handler) newLogAgentLLM(key *database.APIKeyRow, source string, timeout time.Duration) logagent.LLM {
	if h.logAgent.newLLM != nil {
		return h.logAgent.newLLM(h, key, source, timeout)
	}
	return &poolLogAgentLLM{proxy: h.imageProxy, key: key, reason: logAgentInternalReasonPrefix + source, timeout: timeout}
}

// poolLogAgentLLM 经号池（/v1/responses 内部请求）调用模型，用量归属到选定的网关 Key，
// usage_logs.internal_reason 记为 "log_agent:<source>"。
type poolLogAgentLLM struct {
	proxy   *proxy.Handler
	key     *database.APIKeyRow
	reason  string
	timeout time.Duration
}

func (l *poolLogAgentLLM) Respond(ctx context.Context, model, instructions, input string) (string, error) {
	output, _, err := l.RespondWithUsage(ctx, model, instructions, input)
	return output, err
}

func (l *poolLogAgentLLM) RespondWithUsage(ctx context.Context, model, instructions, input string) (string, logagent.Usage, error) {
	if l.proxy == nil {
		return "", logagent.Usage{}, logagent.ErrNoLLM
	}
	body, err := json.Marshal(map[string]any{"model": model, "instructions": instructions, "input": input, "stream": false})
	if err != nil {
		return "", logagent.Usage{}, err
	}
	if l.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}
	status, response := l.proxy.ExecuteInternalResponseForAPIKey(ctx, body, l.key, l.reason)
	usage := logagent.Usage{
		InputTokens:  int(gjson.GetBytes(response, "usage.input_tokens").Int()),
		OutputTokens: int(gjson.GetBytes(response, "usage.output_tokens").Int()),
		TotalTokens:  int(gjson.GetBytes(response, "usage.total_tokens").Int()),
	}
	if status < 200 || status >= 300 {
		if message := strings.TrimSpace(gjson.GetBytes(response, "error.message").String()); message != "" {
			return "", usage, fmt.Errorf("号池模型分析失败: HTTP %d: %s", status, clampRunesText(message, 300))
		}
		return "", usage, fmt.Errorf("号池模型分析失败: HTTP %d", status)
	}
	output := strings.TrimSpace(extractResponseOutputText(response))
	if output == "" {
		return "", usage, errors.New("号池模型没有返回分析结果")
	}
	return output, usage, nil
}
