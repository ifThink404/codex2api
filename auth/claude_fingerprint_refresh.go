package auth

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// ClaudeCustomHeadersPersister 把账号指纹头持久化到凭据存储（由 database.DB 实现）。
type ClaudeCustomHeadersPersister interface {
	UpdateAccountCustomHeaders(ctx context.Context, id int64, headers map[string]string) error
}

// RefreshClaudeFingerprintUserAgent 在指纹 UA 版本低于 targetVersion 时返回只改了版本段的副本。
// UA 缺失、无法识别为 CLI、或版本不低于目标时返回 (原 map, false)。
func RefreshClaudeFingerprintUserAgent(headers map[string]string, targetVersion string) (map[string]string, bool) {
	uaKey := ""
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "user-agent") {
			uaKey = key
			break
		}
	}
	if uaKey == "" {
		return headers, false
	}
	current, ok := ParseClaudeClientVersion(headers[uaKey])
	if !ok {
		return headers, false
	}
	if cmp, err := CompareClaudeClientVersions(current, targetVersion); err != nil || cmp >= 0 {
		return headers, false
	}
	rewritten := RewriteClaudeCLIUserAgentVersion(headers[uaKey], targetVersion)
	if rewritten == "" {
		return headers, false
	}
	next := cloneStringMap(headers)
	next[uaKey] = rewritten
	return next, true
}

// RefreshClaudeFingerprintVersions 把所有 Claude 账号的指纹 UA 版本抬到 version。
// 返回实际改写的账号数与首个持久化错误；单账号失败不影响其它账号。
func RefreshClaudeFingerprintVersions(ctx context.Context, store *Store, persister ClaudeCustomHeadersPersister, version string) (int, error) {
	target, ok := ParseClaudeClientVersion("claude-cli/" + strings.TrimSpace(version))
	if !ok {
		return 0, fmt.Errorf("invalid Claude CLI version %q", version)
	}
	if store == nil {
		return 0, nil
	}
	store.mu.RLock()
	accounts := append([]*Account(nil), store.accounts...)
	store.mu.RUnlock()

	updated := 0
	var firstErr error
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		acc.mu.RLock()
		isClaude := strings.EqualFold(strings.TrimSpace(acc.UpstreamType), UpstreamClaude)
		headers := cloneStringMap(acc.CustomHeaders)
		dbID := acc.DBID
		acc.mu.RUnlock()
		if !isClaude {
			continue
		}
		next, changed := RefreshClaudeFingerprintUserAgent(headers, target)
		if !changed {
			continue
		}
		if persister != nil {
			if err := persister.UpdateAccountCustomHeaders(ctx, dbID, next); err != nil {
				log.Printf("[claude-cli-version-sync] 账号 %d 指纹版本回写失败: %v", dbID, err)
				if firstErr == nil {
					firstErr = fmt.Errorf("account %d: %w", dbID, err)
				}
				continue
			}
		}
		acc.mu.Lock()
		acc.CustomHeaders = next
		acc.mu.Unlock()
		updated++
	}
	return updated, firstErr
}
