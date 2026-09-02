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
		// The DB write above already succeeded, so this account counts as
		// updated regardless of what happens to the in-memory copy below.
		// Between the RLock snapshot read above and this Lock, a concurrent
		// writer (e.g. the store's dispatch-state reconciliation loop calling
		// ApplyAccountCustomHeaders, or an admin edit) may have already
		// changed acc.CustomHeaders. Overwriting it here with our stale
		// `next` would silently lose that update. Guard with a compare-and-
		// swap: only apply `next` if CustomHeaders still matches the
		// snapshot we based it on; otherwise skip the memory write and let
		// the store's own reconciliation converge memory to the DB value
		// (which we just persisted) on its next cycle.
		acc.mu.Lock()
		if stringMapEqual(acc.CustomHeaders, headers) {
			acc.CustomHeaders = next
		} else {
			log.Printf("[claude-cli-version-sync] 账号 %d 指纹在回写期间被并发修改，跳过内存更新", dbID)
		}
		acc.mu.Unlock()
		updated++
	}
	return updated, firstErr
}
