package proxy

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// This is policy metadata only, never an executable/schedulable account.
type missingSessionOwner struct {
	Groups      []int64
	GroupsKnown bool
	UpstreamID  string
	State       string
	Generation  int64
}

func (handler *Handler) readMissingSessionOwner(ctx context.Context, id int64) (*missingSessionOwner, error) {
	if handler.db == nil {
		return nil, errors.New("account database unavailable")
	}
	row, err := handler.db.GetAccountByIDIncludingDeleted(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	groups, err := handler.db.GetAccountGroupIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot := &missingSessionOwner{Groups: groups, GroupsKnown: row != nil || len(groups) > 0, State: "deleted"}
	if row == nil {
		snapshot.State = "not_found"
		return snapshot, nil
	}
	snapshot.UpstreamID, snapshot.Generation = row.GetCredential("account_id"), row.CredentialGeneration
	if strings.EqualFold(row.Status, "deleted") || strings.EqualFold(row.ErrorMessage, "deleted") {
		return snapshot, nil
	}
	snapshot.State = "not_loaded"
	// Only classify explicitly empty Codex credentials here. A runtime reload
	// failure (including failed device-ID persistence) must not masquerade as deletion.
	provider := strings.TrimSpace(row.GetCredential("upstream_type"))
	if (provider == "" || provider == "codex") && row.GetCredential("auth_mode") != auth.CodexAuthModeAgentIdentity &&
		row.GetCredential("access_token") == "" && row.GetCredential("refresh_token") == "" && row.GetCredential("session_token") == "" {
		snapshot.State = "credentials_missing"
	}
	return snapshot, nil
}

func (handler *Handler) resolveMissingSessionOwner(ctx context.Context, id int64) (*auth.Account, *missingSessionOwner, string, error) {
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	snapshot, err := handler.readMissingSessionOwner(lookup, id)
	if err != nil {
		return nil, nil, "lookup_failed", err
	}
	if snapshot.State != "not_loaded" {
		return nil, snapshot, snapshot.State, nil
	}
	if err := handler.store.LoadAccountByID(lookup, id); err != nil {
		return nil, nil, "reload_failed", err
	}
	account := handler.store.FindByID(id)
	if account == nil {
		return nil, nil, "reload_failed", errors.New("reloaded account unavailable")
	}
	return account, nil, "reloaded", nil
}

func (handler *Handler) missingSessionOwnerChange(ctx context.Context, id int64, expected *missingSessionOwner, matchGroups bool) string {
	if expected == nil || handler.store.FindByID(id) != nil {
		return "owner_reappeared"
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	current, err := handler.readMissingSessionOwner(lookup, id)
	if err != nil {
		return "lookup_failed"
	}
	if current.State != expected.State || current.Generation != expected.Generation || current.UpstreamID != expected.UpstreamID || matchGroups && (current.GroupsKnown != expected.GroupsKnown || !slices.Equal(current.Groups, expected.Groups)) {
		return "metadata_changed"
	}
	return ""
}
