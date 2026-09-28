package plugins

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/database"
)

const pluginCaptureRetentionInterval = time.Hour

// CapturePurger deletes expired captures (implemented by *database.DB).
type CapturePurger interface {
	PurgePluginCaptures(ctx context.Context, cutoff time.Time, batchSize int) (database.PluginCapturePurgeResult, error)
}

// PurgeExpiredCaptures deletes captures older than PluginCaptureRetention.
func PurgeExpiredCaptures(ctx context.Context, purger CapturePurger, now time.Time) (database.PluginCapturePurgeResult, error) {
	return purger.PurgePluginCaptures(ctx, now.Add(-PluginCaptureRetention), database.DefaultPluginCapturePurgeBatch)
}

// StartPluginCaptureRetention purges expired plugin captures hourly (first run
// after a short delay, like the prompt log retention job) until ctx ends.
func StartPluginCaptureRetention(ctx context.Context, purger CapturePurger) {
	if purger == nil {
		return
	}
	go func() {
		run := func() {
			start := time.Now()
			result, err := PurgeExpiredCaptures(ctx, purger, start)
			if err != nil {
				log.Printf("[transport-plugin] capture retention purge failed: %v", err)
				return
			}
			if result.Deleted > 0 {
				log.Printf("[transport-plugin] capture retention purged %d rows in %d batches (%s)", result.Deleted, result.Batches, time.Since(start).Round(time.Millisecond))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
			run()
		}
		ticker := time.NewTicker(pluginCaptureRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
