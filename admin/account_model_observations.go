package admin

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func (h *Handler) attachAccountModelObservations(ctx context.Context, accounts []accountResponse) {
	if h == nil || h.db == nil || len(accounts) == 0 {
		return
	}
	ids := make([]int64, 0, len(accounts))
	for _, a := range accounts {
		if !a.OpenAIResponsesAPI && !a.GrokAPI && !a.ClaudeAPI && !a.AntigravityAPI {
			ids = append(ids, a.ID)
		}
	}
	observations, err := h.db.ListAccountModelObservations(ctx, ids)
	if err != nil {
		log.Printf("account model observations: %v", err)
		return
	}
	for i := range accounts {
		accounts[i].ModelObservations = observations[accounts[i].ID]
	}
}

func (h *Handler) recordAccountManifestModels(ctx context.Context, account *auth.Account, generation int64, observedAt time.Time, models []string) error {
	if h == nil || h.db == nil || account == nil || account.IsRelayStyle() {
		return nil
	}
	observations := make([]database.AccountModelObservation, 0, len(models))
	for _, model := range models {
		observations = append(observations, database.AccountModelObservation{Model: model, Transport: "codex", Source: "manifest", Outcome: "listed", ObservedAt: observedAt.Unix()})
	}
	return h.db.SaveAccountModelObservations(ctx, account.ID(), generation, observations)
}
