package auth

import (
	"encoding/json"
	"strings"
	"sync/atomic"
)

// Claude Code 出站请求的指纹收敛模式(账号级;空值 = 跟随全局默认):
//
//	preserve — 入站真实客户端身份头优先,缺失才用账号绑定指纹补齐(历史默认行为)。
//	force    — 无条件用账号绑定指纹覆盖入站身份头(强制替换,保证同一账号
//	           对 Anthropic 始终呈现同一套 Claude Code 身份)。
const (
	ClaudeFingerprintModePreserve = "preserve"
	ClaudeFingerprintModeForce    = "force"
)

// ClaudeFingerprintModeCredentialKey 是该模式在账号 credentials 中的存储键。
const ClaudeFingerprintModeCredentialKey = "claude_fingerprint_mode"

// NormalizeClaudeFingerprintMode 归一化模式取值;空/非法值归一为空串(跟随全局)。
func NormalizeClaudeFingerprintMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ClaudeFingerprintModePreserve:
		return ClaudeFingerprintModePreserve
	case ClaudeFingerprintModeForce:
		return ClaudeFingerprintModeForce
	}
	return ""
}

// IsValidClaudeFingerprintMode 报告取值是否合法(空串=跟随全局,亦视为合法)。
func IsValidClaudeFingerprintMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ClaudeFingerprintModePreserve, ClaudeFingerprintModeForce:
		return true
	}
	return false
}

// EffectiveClaudeFingerprintMode 返回账号生效模式:账号级覆盖 > 全局默认 > preserve。
func (a *Account) EffectiveClaudeFingerprintMode(globalDefault string) string {
	if a != nil {
		a.mu.RLock()
		mode := a.ClaudeFingerprintMode
		a.mu.RUnlock()
		if m := NormalizeClaudeFingerprintMode(mode); m != "" {
			return m
		}
	}
	if m := NormalizeClaudeFingerprintMode(globalDefault); m != "" {
		return m
	}
	return ClaudeFingerprintModePreserve
}

// ── Claude 全局配置访问器(来自系统设置 claude_config,ApplySystemSettings 注入) ──

// SetClaudeFingerprintModeDefault 设置 Claude 指纹模式全局默认。
func (s *Store) SetClaudeFingerprintModeDefault(mode string) {
	s.claudeFingerprintDefault.Store(NormalizeClaudeFingerprintMode(mode))
}

// ClaudeFingerprintModeDefault 返回 Claude 指纹模式全局默认(空=preserve)。
func (s *Store) ClaudeFingerprintModeDefault() string {
	if v, ok := s.claudeFingerprintDefault.Load().(string); ok {
		return v
	}
	return ""
}

// SetClaudeDefaultTimezone 设置导入 Claude 账号的默认时区。
func (s *Store) SetClaudeDefaultTimezone(tz string) {
	s.claudeDefaultTimezone.Store(strings.TrimSpace(tz))
}

// ClaudeDefaultTimezone 返回导入 Claude 账号的默认时区(空=不指定)。
func (s *Store) ClaudeDefaultTimezone() string {
	if v, ok := s.claudeDefaultTimezone.Load().(string); ok {
		return v
	}
	return ""
}

// SetClaudeSessionWindowLimit 设置 Claude 账号默认并发会话窗口数(<=0 归 0=跟随全局)。
func (s *Store) SetClaudeSessionWindowLimit(n int64) {
	if n < 0 {
		n = 0
	}
	atomic.StoreInt64(&s.claudeSessionWindowLimit, n)
}

// ClaudeSessionWindowLimit 返回 Claude 账号默认并发会话窗口数(0=跟随全局 maxConcurrency)。
func (s *Store) ClaudeSessionWindowLimit() int64 {
	return atomic.LoadInt64(&s.claudeSessionWindowLimit)
}

// ApplyAccountClaudeFingerprintMode 更新内存态账号的 Claude 指纹模式。
func (s *Store) ApplyAccountClaudeFingerprintMode(dbID int64, mode string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.ClaudeFingerprintMode = NormalizeClaudeFingerprintMode(mode)
	acc.mu.Unlock()
	return true
}

// claudeSessionWindowForRow 仅对 Claude 账号返回全局并发会话窗口默认;其它渠道返回 0。
func claudeSessionWindowForRow(upstreamType string, globalWindow int64) int64 {
	if globalWindow > 0 && strings.EqualFold(strings.TrimSpace(upstreamType), UpstreamClaude) {
		return globalWindow
	}
	return 0
}

// ClaudeConfig 是 ClaudeCode 全局配置(系统设置 claude_config 列反序列化目标)。
// 全体 Claude 账号默认遵守;个体账号可通过编辑覆盖。
type ClaudeConfig struct {
	FingerprintMode    string `json:"fingerprint_mode"`     // preserve / force(空=preserve)
	DefaultTimezone    string `json:"default_timezone"`     // 导入账号默认 IANA 时区
	SessionWindowLimit int64  `json:"session_window_limit"` // 默认并发会话窗口数(0=跟随全局 maxConcurrency)
}

// ParseClaudeConfig 解析 claude_config JSON;空/非法回落到零值(即全部默认)。
func ParseClaudeConfig(raw string) ClaudeConfig {
	var cfg ClaudeConfig
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return cfg
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	cfg.FingerprintMode = NormalizeClaudeFingerprintMode(cfg.FingerprintMode)
	cfg.DefaultTimezone = strings.TrimSpace(cfg.DefaultTimezone)
	if cfg.SessionWindowLimit < 0 {
		cfg.SessionWindowLimit = 0
	}
	return cfg
}

// applyClaudeConfigToStore 把解析后的 ClaudeCode 全局配置写入 Store 的运行时访问器。
func applyClaudeConfigToStore(s *Store, raw string) {
	cfg := ParseClaudeConfig(raw)
	s.SetClaudeFingerprintModeDefault(cfg.FingerprintMode)
	s.SetClaudeDefaultTimezone(cfg.DefaultTimezone)
	s.SetClaudeSessionWindowLimit(cfg.SessionWindowLimit)
}
