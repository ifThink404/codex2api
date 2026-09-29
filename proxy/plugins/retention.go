package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/codex2api/database"
)

// Capture retention. Every plugin config may set two windows (in hours):
// capture_error_retention_hours for error captures and the other captures of
// their attempt (default and maximum 12), and capture_retention_hours for
// the rest (default 6, 1 to 12). The job runs every 10 minutes with batched
// deletes; rows of any plugin older than the 12h maximum always go.

const (
	pluginCaptureRetentionInterval = 10 * time.Minute
	pluginCaptureRetentionDelay    = 2 * time.Minute
)

// CaptureRetention is one plugin's two retention windows.
type CaptureRetention struct {
	Normal time.Duration
	Error  time.Duration
}

// ParseCaptureRetention reads the retention windows from a plugin config,
// applying the defaults for absent keys.
func ParseCaptureRetention(config json.RawMessage) (CaptureRetention, error) {
	retention := CaptureRetention{Normal: database.DefaultPluginCaptureRetention, Error: database.DefaultPluginCaptureErrorRetention}
	var raw struct {
		Normal *int `json:"capture_retention_hours"`
		Error  *int `json:"capture_error_retention_hours"`
	}
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &raw); err != nil {
			return retention, nil // the plugin's own validator reports malformed configs
		}
	}
	window := func(name string, hours *int, target *time.Duration) error {
		if hours == nil || *hours == 0 {
			return nil // absent or 0: the default, as for other numeric plugin settings
		}
		d := time.Duration(*hours) * time.Hour
		if d < database.PluginCaptureMinRetention || d > database.PluginCaptureMaxRetention {
			return fmt.Errorf("%s must be between %d and %d hours", name, int(database.PluginCaptureMinRetention/time.Hour), int(database.PluginCaptureMaxRetention/time.Hour))
		}
		*target = d
		return nil
	}
	if err := window("capture_retention_hours", raw.Normal, &retention.Normal); err != nil {
		return retention, err
	}
	if err := window("capture_error_retention_hours", raw.Error, &retention.Error); err != nil {
		return retention, err
	}
	return retention, nil
}

// CapturePurger deletes expired captures (implemented by *database.DB).
type CapturePurger interface {
	PurgePluginCaptures(ctx context.Context, cutoff time.Time, batchSize int) (database.PluginCapturePurgeResult, error)
	PurgePluginCaptureWindows(ctx context.Context, plugin string, normalCutoff, errorCutoff time.Time, batchSize int) (database.PluginCapturePurgeResult, error)
	VacuumPluginCaptures(ctx context.Context) error
}

// PurgeExpiredCaptures applies every registered plugin's windows, then the
// global 12h maximum (which also covers rows of plugins no longer compiled
// in), and vacuums after a large purge.
func (r *Registry) PurgeExpiredCaptures(ctx context.Context, purger CapturePurger, now time.Time) (database.PluginCapturePurgeResult, error) {
	var total database.PluginCapturePurgeResult
	add := func(result database.PluginCapturePurgeResult) {
		total.Deleted += result.Deleted
		total.Batches += result.Batches
		total.Interrupted = total.Interrupted || result.Interrupted
	}
	for _, p := range r.Plugins() {
		retention, _ := ParseCaptureRetention(r.State(p.ID()).Config)
		result, err := purger.PurgePluginCaptureWindows(ctx, p.ID(), now.Add(-retention.Normal), now.Add(-retention.Error), database.DefaultPluginCapturePurgeBatch)
		add(result)
		if err != nil || result.Interrupted {
			return total, err
		}
	}
	result, err := purger.PurgePluginCaptures(ctx, now.Add(-database.PluginCaptureMaxRetention), database.DefaultPluginCapturePurgeBatch)
	add(result)
	if err == nil && total.Deleted >= database.PluginCaptureVacuumThreshold {
		if vacuumErr := purger.VacuumPluginCaptures(ctx); vacuumErr != nil {
			log.Printf("[transport-plugin] capture vacuum failed: %v", vacuumErr)
		}
	}
	return total, err
}

// RunMaintenance runs every plugin's Maintainer (e.g. BPS identity pruning).
func (r *Registry) RunMaintenance(ctx context.Context, db *database.DB, now time.Time) {
	for _, p := range r.Plugins() {
		if m, ok := p.(Maintainer); ok {
			if err := m.Maintain(ctx, db, now); err != nil {
				log.Printf("[transport-plugin] %s maintenance failed: %v", p.ID(), err)
			}
		}
	}
}

// StartPluginCaptureRetention purges expired plugin captures every 10 minutes
// (first run after a short delay) until ctx ends, and runs the plugins'
// maintenance in the same pass.
func StartPluginCaptureRetention(ctx context.Context, purger CapturePurger) {
	if purger == nil {
		return
	}
	go func() {
		run := func() {
			start := time.Now()
			result, err := Default().PurgeExpiredCaptures(ctx, purger, start)
			if err != nil {
				log.Printf("[transport-plugin] capture retention purge failed: %v", err)
			}
			if result.Deleted > 0 {
				log.Printf("[transport-plugin] capture retention purged %d rows in %d batches (%s)", result.Deleted, result.Batches, time.Since(start).Round(time.Millisecond))
			}
			if db, ok := purger.(*database.DB); ok {
				Default().RunMaintenance(ctx, db, start)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pluginCaptureRetentionDelay):
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
