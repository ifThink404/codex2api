package proxy

import (
	"context"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"strings"
	"time"
)

// Supplement a sampled manifest only with fresh evidence from accounts visible
// to this key. Global discovery alone is not proof of account access.
func (h *Handler) observedCodexManifestModels(ctx context.Context, row *database.APIKeyRow) map[string]bool {
	out := make(map[string]bool)
	if h == nil || h.db == nil || h.store == nil || row == nil {
		return out
	}
	now := time.Now()
	ids := []int64{}
	accounts := map[int64]*auth.Account{}
	for _, a := range h.store.Accounts() {
		if a != nil && !a.IsRelayStyle() && h.accountVisibleToAPIKey(a, row.ID, now) {
			ids = append(ids, a.ID())
			accounts[a.ID()] = a
		}
	}
	observations, err := h.db.ListAccountModelObservations(ctx, ids)
	if err != nil {
		return out
	}
	for id, rows := range observations {
		a := accounts[id]
		transport := "codex"
		if a.CodexBPSEnabled() {
			transport = "bps"
		}
		newest := map[string]database.AccountModelObservation{}
		for _, o := range rows {
			if o.Transport != transport || now.Unix()-o.ObservedAt > 86400 || o.ObservedAt > now.Unix()+60 {
				continue
			}
			key := strings.ToLower(o.Model)
			old, exists := newest[key]
			if !exists || o.ObservedAt > old.ObservedAt || (o.ObservedAt == old.ObservedAt && o.Source == "probe") {
				newest[key] = o
			}
		}
		for model, o := range newest {
			if (o.Outcome == "listed" || o.Outcome == "available") && a.SupportsCodexModel(model) {
				out[model] = true
			}
		}
	}
	return out
}
