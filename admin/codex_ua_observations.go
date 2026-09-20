package admin

import (
	"context"
	"time"

	"github.com/codex2api/proxy"
)

type codexUAObservationCache struct {
	index     *proxy.CodexUserAgentObservations
	expiresAt time.Time
}

func (h *Handler) codexUserAgentObservations(ctx context.Context) *proxy.CodexUserAgentObservations {
	h.codexUAObservationsMu.Lock()
	defer h.codexUAObservationsMu.Unlock()
	if cached := h.codexUAObservations; cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.index
	}
	if h.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	samples, err := h.db.RecentUsageClientUserAgents(ctx)
	if err != nil {
		// A failed query is not evidence that a version pair is unseen.
		h.codexUAObservations = &codexUAObservationCache{expiresAt: time.Now().Add(5 * time.Second)}
		return nil
	}
	now := time.Now()
	index := proxy.NewCodexUserAgentObservations(samples, now)
	h.codexUAObservations = &codexUAObservationCache{index: index, expiresAt: now.Add(time.Minute)}
	return index
}
