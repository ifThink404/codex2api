package database

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
)

// Projects survive thread changes and A -> B -> A failover. Their namespace
// deliberately excludes roots and epochs, but retains caller and account scope.
func projectBinding(binding CodexTurnStateBinding) CodexTurnStateBinding {
	binding.RootKey = "project-account-v1"
	binding.Generation = 0
	return binding
}

func (db *DB) projectRegistryKey(scope, original string) string {
	return hex.EncodeToString(db.responseIDMAC("project-registry-v1", []byte(scope+"\x00"+original)))
}

func (db *DB) RegisterCodexProjectID(ctx context.Context, scope, original string) error {
	parsed, err := uuid.Parse(original)
	if err != nil || parsed.String() != original || parsed == uuid.Nil || scope == "" || db.turnStateCipher == nil {
		return errors.New("invalid project identity registration")
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO codex_project_registry(source_key) VALUES($1) ON CONFLICT DO NOTHING`, db.projectRegistryKey(scope, original))
	if err == nil {
		_, err = db.conn.ExecContext(ctx, `INSERT INTO codex_project_registry(source_key) VALUES($1) ON CONFLICT DO NOTHING`, db.projectRegistryKey(scope, "registered-projects"))
	}
	return err
}

func (db *DB) HasCodexProjectMappings(ctx context.Context, binding CodexTurnStateBinding) (bool, error) {
	var found bool
	err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM codex_project_registry WHERE source_key=$1)`, db.protocolKey(projectBinding(binding), "project", "present", "")).Scan(&found)
	return found, err
}

func (db *DB) IsKnownCodexProjectID(ctx context.Context, scope, original string) (bool, error) {
	var known bool
	err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM codex_project_registry WHERE source_key=$1)`, db.projectRegistryKey(scope, original)).Scan(&known)
	return known, err
}

func (db *DB) ResolveCodexProjectID(ctx context.Context, binding CodexTurnStateBinding, original string) (CodexProtocolPair, error) {
	binding = projectBinding(binding)
	if pair, found, err := db.ReadCodexProtocolPair(ctx, binding, "project", original, true); err != nil || found {
		if err == nil {
			_, err = db.conn.ExecContext(ctx, `INSERT INTO codex_project_registry(source_key) VALUES($1) ON CONFLICT DO NOTHING`, db.protocolKey(binding, "project", "present", ""))
		}
		return pair, err
	}
	known, err := db.IsKnownCodexProjectID(ctx, binding.Scope, original)
	if err != nil || !known {
		if err == nil {
			err = errors.New("unregistered project identity")
		}
		return CodexProtocolPair{}, err
	}
	key := db.protocolKey(binding, "project", "alias", original)
	alias, err := db.ResolveCodexIdentityUUIDv7(ctx, key, key)
	if err != nil {
		return CodexProtocolPair{}, err
	}
	pair := CodexProtocolPair{Public: original, Upstream: alias}
	if err := db.PutCodexProtocolPair(ctx, binding, "project", pair); err != nil {
		return CodexProtocolPair{}, err
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO codex_project_registry(source_key) VALUES($1) ON CONFLICT DO NOTHING`, db.protocolKey(binding, "project", "present", "")); err != nil {
		return CodexProtocolPair{}, err
	}
	return pair, nil
}

func (db *DB) RestoreCodexProjectID(ctx context.Context, binding CodexTurnStateBinding, alias string) (CodexProtocolPair, bool, error) {
	return db.ReadCodexProtocolPair(ctx, projectBinding(binding), "project", alias, false)
}
