package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// 模型目录页「刷新账号模型」的统一入口：一次刷新所有渠道，并把各渠道刷出来的
// 模型写回各自的真源（Codex → model_registry，Claude/Grok/Antigravity → 账号凭据），
// 使目录页、/v1/models 与调度准入看到同一份清单。

const (
	modelRefreshAllTimeout        = 120 * time.Second
	modelRefreshGrokConcurrency   = 2
	modelRefreshAntigravityWorker = 2
)

// modelRefreshChannelOrder 固定渠道展示顺序，与定价页分组顺序一致。
var modelRefreshChannelOrder = []string{
	database.UpstreamChannelCodex,
	database.UpstreamChannelClaude,
	database.UpstreamChannelGrok,
	database.UpstreamChannelAntigravity,
}

// channelModelRefreshResult 是单个渠道的刷新结果。
type channelModelRefreshResult struct {
	Channel   string   `json:"channel"`
	Refreshed int      `json:"refreshed"`       // 成功刷新（写回）的账号数；Codex 含官方页同步计 1
	Failed    int      `json:"failed"`          // 拉取或写回失败的账号数
	Added     []string `json:"added"`           // 本次新出现在目录里的模型
	Error     string   `json:"error,omitempty"` // 渠道级失败原因（账号级失败只计数）
}

type refreshAllModelsResponse struct {
	Message    string                      `json:"message"`
	Channels   []channelModelRefreshResult `json:"channels"`
	Added      []string                    `json:"added"`
	ModelCount int                         `json:"model_count"`
	DurationMs int64                       `json:"duration_ms"`
}

// channelModelRefreshFunc 是单渠道刷新实现；测试可通过 Handler.modelRefreshFuncs 注入。
type channelModelRefreshFunc func(ctx context.Context) channelModelRefreshResult

// RefreshAllModels 并行刷新所有渠道的可用模型（POST /api/admin/models/refresh-all）。
// 任一渠道失败不影响其他渠道写入；单渠道失败在对应 channel.error 中报告，整体仍 200。
func (h *Handler) RefreshAllModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), modelRefreshAllTimeout)
	defer cancel()
	c.JSON(http.StatusOK, h.runRefreshAllModels(ctx))
}

func (h *Handler) runRefreshAllModels(ctx context.Context) refreshAllModelsResponse {
	started := time.Now()
	funcs := h.modelRefreshFuncs
	if funcs == nil {
		funcs = h.defaultModelRefreshFuncs()
	}

	results := make(map[string]channelModelRefreshResult, len(funcs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for channel, fn := range funcs {
		wg.Add(1)
		go func(channel string, fn channelModelRefreshFunc) {
			defer wg.Done()
			result := runChannelModelRefresh(ctx, channel, fn)
			mu.Lock()
			results[channel] = result
			mu.Unlock()
		}(channel, fn)
	}
	wg.Wait()

	resp := refreshAllModelsResponse{
		Message:  "已刷新各渠道可用模型",
		Channels: make([]channelModelRefreshResult, 0, len(results)),
		Added:    make([]string, 0),
	}
	for _, channel := range orderedModelRefreshChannels(results) {
		result := results[channel]
		if result.Added == nil {
			result.Added = []string{}
		}
		resp.Channels = append(resp.Channels, result)
		resp.Added = append(resp.Added, result.Added...)
	}
	sort.Strings(resp.Added)
	resp.ModelCount = len(h.modelPricingCatalogKeys(ctx))
	resp.DurationMs = time.Since(started).Milliseconds()
	return resp
}

// runChannelModelRefresh 保证单渠道 panic 或超时不拖垮整体响应。
func runChannelModelRefresh(ctx context.Context, channel string, fn channelModelRefreshFunc) (result channelModelRefreshResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = channelModelRefreshResult{Channel: channel, Error: fmt.Sprintf("panic: %v", recovered)}
		}
		result.Channel = channel
		if result.Error == "" && ctx.Err() != nil && result.Refreshed == 0 {
			result.Error = ctx.Err().Error()
		}
	}()
	return fn(ctx)
}

func orderedModelRefreshChannels(results map[string]channelModelRefreshResult) []string {
	ordered := make([]string, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, channel := range modelRefreshChannelOrder {
		if _, ok := results[channel]; ok {
			ordered = append(ordered, channel)
			seen[channel] = struct{}{}
		}
	}
	extras := make([]string, 0)
	for channel := range results {
		if _, ok := seen[channel]; !ok {
			extras = append(extras, channel)
		}
	}
	sort.Strings(extras)
	return append(ordered, extras...)
}

func (h *Handler) defaultModelRefreshFuncs() map[string]channelModelRefreshFunc {
	return map[string]channelModelRefreshFunc{
		database.UpstreamChannelCodex:       h.refreshCodexChannelModels,
		database.UpstreamChannelClaude:      h.refreshClaudeChannelModels,
		database.UpstreamChannelGrok:        h.refreshGrokChannelModels,
		database.UpstreamChannelAntigravity: h.refreshAntigravityChannelModels,
	}
}

// modelPricingCatalogKeys 返回定价页/模型目录当前展示的全部规范模型键（各渠道拼接），
// 与 ListModelPricing 的口径一致。
func (h *Handler) modelPricingCatalogKeys(ctx context.Context) []string {
	keys := modelPricingManagementKeys(proxy.SupportedModelIDs(ctx, h.db))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		seen[key] = struct{}{}
	}
	appendUnique := func(ids []string) {
		for _, key := range modelPricingManagementKeys(ids) {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	appendUnique(append(h.grokBillingModelIDs(), grokDefaultDisplayModelIDs()...))
	appendUnique(h.antigravityChannelModels())
	appendUnique(h.claudeChannelModels())
	return keys
}

// newlyAddedModels 返回 after 中有而 before 中没有的模型（忽略大小写），已排序。
func newlyAddedModels(before, after []string) []string {
	known := make(map[string]struct{}, len(before))
	for _, id := range before {
		known[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	added := make([]string, 0)
	seen := make(map[string]struct{})
	for _, id := range after {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, ok := known[key]; ok {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		added = append(added, strings.TrimSpace(id))
	}
	sort.Strings(added)
	return added
}

// ==================== Codex ====================

// isCodexOAuthAccount 判断账号是否为 ChatGPT OAuth 的 Codex 官方账号
// （非 relay 中转、非 Grok/Claude/Antigravity）。
func isCodexOAuthAccount(account *auth.Account) bool {
	if account == nil {
		return false
	}
	return !account.IsRelayStyle() && !account.IsAntigravityAPI() && !account.IsGrokAPI() && !account.IsClaudeOAuth()
}

// codexManifestSampleAccounts 每种套餐（plan_type）挑一个可调度的 Codex OAuth 账号：
// 同套餐账号看到的上游清单相同，逐个拉只会放大上游请求量。
func (h *Handler) codexManifestSampleAccounts() []*auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	byPlan := make(map[string]*auth.Account)
	for _, account := range h.store.Accounts() {
		if !isCodexOAuthAccount(account) || atomic.LoadInt32(&account.Disabled) != 0 {
			continue
		}
		account.Mu().RLock()
		status := account.Status
		account.Mu().RUnlock()
		if status == auth.StatusError {
			continue
		}
		plan := auth.NormalizePlanType(account.GetPlanType())
		if plan == "" {
			plan = "unknown"
		}
		if _, ok := byPlan[plan]; !ok {
			byPlan[plan] = account
		}
	}
	plans := make([]string, 0, len(byPlan))
	for plan := range byPlan {
		plans = append(plans, plan)
	}
	sort.Strings(plans)
	accounts := make([]*auth.Account, 0, len(plans))
	for _, plan := range plans {
		accounts = append(accounts, byPlan[plan])
	}
	return accounts
}

func (h *Handler) refreshCodexChannelModels(ctx context.Context) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelCodex, Added: []string{}}
	if h == nil || h.db == nil {
		result.Error = "数据库不可用"
		return result
	}
	before := proxy.SupportedModelIDs(ctx, h.db)

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	// 1. 官方模型页 → 注册表（与设置页「同步上游模型」同一实现）。
	if _, err := proxy.SyncOfficialCodexModels(ctx, h.db, proxyURL); err != nil {
		result.Error = fmt.Sprintf("官方模型页同步失败: %v", err)
		result.Failed++
	} else {
		result.Refreshed++
	}

	// 2. 各套餐账号的上游清单 → 注册表（只增不改不删，沿用 LearnModelsFromManifest）。
	now := time.Now().UTC()
	for _, account := range h.codexManifestSampleAccounts() {
		if ctx.Err() != nil {
			break
		}
		manifest, err := proxy.FetchCodexModelsManifest(ctx, account, h.store.ResolveProxyForAccount(account), "", "")
		if err != nil {
			result.Failed++
			continue
		}
		proxy.RecordResponsesLiteSupportFromManifest(manifest.Body)
		if _, err := proxy.LearnModelsFromManifest(ctx, h.db, manifest.Body, now); err != nil {
			result.Failed++
			continue
		}
		result.Refreshed++
	}

	result.Added = newlyAddedModels(before, proxy.SupportedModelIDs(ctx, h.db))
	return result
}

// ==================== Claude ====================

func (h *Handler) refreshClaudeChannelModels(ctx context.Context) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelClaude, Added: []string{}}
	if h == nil || h.db == nil {
		result.Error = "数据库不可用"
		return result
	}
	before := h.claudeChannelModels()
	refreshed, failed, err := h.refreshAllClaudeModels(ctx)
	result.Refreshed = refreshed
	result.Failed = failed
	if err != nil {
		result.Error = err.Error()
	}
	result.Added = newlyAddedModels(before, h.claudeChannelModels())
	return result
}

// ==================== Grok ====================

func (h *Handler) refreshGrokChannelModels(ctx context.Context) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelGrok, Added: []string{}}
	if h == nil || h.store == nil {
		return result
	}
	before := append(h.grokBillingModelIDs(), grokDefaultDisplayModelIDs()...)
	ids := make([]int64, 0)
	for _, account := range h.store.Accounts() {
		if account == nil || !account.IsGrokAPI() || atomic.LoadInt32(&account.Disabled) != 0 {
			continue
		}
		ids = append(ids, account.ID())
	}
	h.refreshAccountsWithWorkers(ctx, ids, modelRefreshGrokConcurrency, &result, func(ctx context.Context, id int64) bool {
		syncResult, err := h.syncGrokAccountState(ctx, id)
		if err != nil {
			return false
		}
		if syncResult != nil && syncResult.capabilityGeneration > 0 {
			h.triggerGrokCapabilityProbeForGeneration(id, syncResult.capabilityGeneration)
		}
		return true
	})
	result.Added = newlyAddedModels(before, append(h.grokBillingModelIDs(), grokDefaultDisplayModelIDs()...))
	return result
}

// ==================== Antigravity ====================

func (h *Handler) refreshAntigravityChannelModels(ctx context.Context) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelAntigravity, Added: []string{}}
	if h == nil || h.store == nil {
		return result
	}
	before := h.antigravityChannelModels()
	ids := make([]int64, 0)
	for _, account := range h.store.Accounts() {
		if account == nil || !account.IsAntigravityAPI() || atomic.LoadInt32(&account.Disabled) != 0 {
			continue
		}
		ids = append(ids, account.ID())
	}
	h.refreshAccountsWithWorkers(ctx, ids, modelRefreshAntigravityWorker, &result, func(ctx context.Context, id int64) bool {
		return h.runAntigravityRefresh(ctx, id).OK
	})
	result.Added = newlyAddedModels(before, h.antigravityChannelModels())
	return result
}

// refreshAccountsWithWorkers 用有限并发逐账号执行 refresh，成功/失败计入 result。
func (h *Handler) refreshAccountsWithWorkers(ctx context.Context, ids []int64, workers int, result *channelModelRefreshResult, refresh func(ctx context.Context, id int64) bool) {
	if len(ids) == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int64)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				ok := false
				if ctx.Err() == nil {
					ok = refresh(ctx, id)
				}
				mu.Lock()
				if ok {
					result.Refreshed++
				} else {
					result.Failed++
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
}
