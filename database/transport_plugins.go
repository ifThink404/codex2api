package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Transport plugin state (see proxy/plugins). One row per compiled-in plugin;
// a missing row means "defaults": disabled, no groups, empty config, capture
// off. Every write appends a scheduler_outbox 'plugin' event in the same
// transaction so all replicas reload the snapshot.
const transportPluginsSchemaPostgres = `
	CREATE TABLE IF NOT EXISTS transport_plugins (
		id                  VARCHAR(32) PRIMARY KEY,
		enabled             BOOLEAN NOT NULL DEFAULT FALSE,
		group_ids           TEXT NOT NULL DEFAULT '[]',
		config              TEXT NOT NULL DEFAULT '{}',
		capture_enabled     BOOLEAN NOT NULL DEFAULT FALSE,
		capture_sample_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
		updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS plugin_captures (
		id          BIGSERIAL PRIMARY KEY,
		plugin      VARCHAR(32) NOT NULL,
		request_id  VARCHAR(128) NOT NULL DEFAULT '',
		account_id  BIGINT NOT NULL DEFAULT 0,
		attempt     INT NOT NULL DEFAULT 0,
		direction   VARCHAR(16) NOT NULL,
		status      INT NOT NULL DEFAULT 0,
		headers     TEXT NOT NULL DEFAULT '',
		body        TEXT NOT NULL DEFAULT '',
		error_kind  VARCHAR(64) NOT NULL DEFAULT '',
		truncated   BOOLEAN NOT NULL DEFAULT FALSE,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_plugin_captures_created ON plugin_captures(created_at, id);
	CREATE INDEX IF NOT EXISTS idx_plugin_captures_plugin_created ON plugin_captures(plugin, created_at);
	CREATE INDEX IF NOT EXISTS idx_plugin_captures_request ON plugin_captures(request_id) WHERE request_id <> '';
`

const transportPluginsSchemaSQLite = `
	CREATE TABLE IF NOT EXISTS transport_plugins (
		id                  TEXT PRIMARY KEY,
		enabled             INTEGER NOT NULL DEFAULT 0,
		group_ids           TEXT NOT NULL DEFAULT '[]',
		config              TEXT NOT NULL DEFAULT '{}',
		capture_enabled     INTEGER NOT NULL DEFAULT 0,
		capture_sample_rate REAL NOT NULL DEFAULT 0,
		updated_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS plugin_captures (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		plugin      TEXT NOT NULL,
		request_id  TEXT NOT NULL DEFAULT '',
		account_id  INTEGER NOT NULL DEFAULT 0,
		attempt     INTEGER NOT NULL DEFAULT 0,
		direction   TEXT NOT NULL,
		status      INTEGER NOT NULL DEFAULT 0,
		headers     TEXT NOT NULL DEFAULT '',
		body        TEXT NOT NULL DEFAULT '',
		error_kind  TEXT NOT NULL DEFAULT '',
		truncated   INTEGER NOT NULL DEFAULT 0,
		created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_plugin_captures_created ON plugin_captures(created_at, id);
	CREATE INDEX IF NOT EXISTS idx_plugin_captures_plugin_created ON plugin_captures(plugin, created_at);
	CREATE INDEX IF NOT EXISTS idx_plugin_captures_request ON plugin_captures(request_id) WHERE request_id <> '';
`

// transportPluginIDPattern keeps plugin IDs usable as credential-key and
// usage_logs.transport fragments ([a-z0-9_] only, see sqliteJSONSetKeySupported).
var transportPluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidTransportPluginID reports whether id is a syntactically valid plugin ID.
// "native" is reserved for the built-in transport.
func ValidTransportPluginID(id string) bool {
	return id != TransportNative && transportPluginIDPattern.MatchString(id)
}

// TransportNative is the usage_logs.transport value for requests that did not
// go through a transport plugin.
const TransportNative = "native"

type TransportPluginState struct {
	ID                string          `json:"id"`
	Enabled           bool            `json:"enabled"`
	GroupIDs          []int64         `json:"group_ids"`
	Config            json.RawMessage `json:"config"`
	CaptureEnabled    bool            `json:"capture_enabled"`
	CaptureSampleRate float64         `json:"capture_sample_rate"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// NormalizeTransportPluginState validates and canonicalizes a state row in
// place: positive unique group IDs, a JSON object config and a sample rate in
// [0, 1].
func NormalizeTransportPluginState(state *TransportPluginState) error {
	if state == nil {
		return fmt.Errorf("transport plugin state is required")
	}
	state.ID = strings.TrimSpace(state.ID)
	if !ValidTransportPluginID(state.ID) {
		return fmt.Errorf("invalid transport plugin id %q", state.ID)
	}
	state.GroupIDs = normalizePositiveInt64Slice(state.GroupIDs)
	config := strings.TrimSpace(string(state.Config))
	if config == "" || config == "null" {
		config = "{}"
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &object); err != nil || object == nil {
		return fmt.Errorf("transport plugin config must be a JSON object")
	}
	state.Config = json.RawMessage(config)
	if state.CaptureSampleRate != state.CaptureSampleRate || state.CaptureSampleRate < 0 || state.CaptureSampleRate > 1 {
		return fmt.Errorf("capture_sample_rate must be between 0 and 1")
	}
	return nil
}

func (db *DB) ListTransportPluginStates(ctx context.Context) ([]TransportPluginState, error) {
	if db == nil || db.conn == nil {
		return nil, nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id, enabled, group_ids, config, capture_enabled, capture_sample_rate, updated_at FROM transport_plugins ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var states []TransportPluginState
	for rows.Next() {
		var state TransportPluginState
		var groupsRaw, configRaw string
		var updatedRaw any
		if err := rows.Scan(&state.ID, &state.Enabled, &groupsRaw, &configRaw, &state.CaptureEnabled, &state.CaptureSampleRate, &updatedRaw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(groupsRaw), &state.GroupIDs); err != nil {
			state.GroupIDs = nil
		}
		state.GroupIDs = normalizePositiveInt64Slice(state.GroupIDs)
		state.Config = json.RawMessage(configRaw)
		if !json.Valid(state.Config) {
			state.Config = json.RawMessage(`{}`)
		}
		state.UpdatedAt, _ = parseDBTimeValue(updatedRaw)
		states = append(states, state)
	}
	return states, rows.Err()
}

// SaveTransportPluginState upserts a plugin row and emits the 'plugin' outbox
// event in the same transaction.
func (db *DB) SaveTransportPluginState(ctx context.Context, state TransportPluginState) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database is not initialized")
	}
	if err := NormalizeTransportPluginState(&state); err != nil {
		return err
	}
	groups, err := json.Marshal(state.GroupIDs)
	if err != nil {
		return err
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO transport_plugins (id, enabled, group_ids, config, capture_enabled, capture_sample_rate, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, CURRENT_TIMESTAMP)
			ON CONFLICT (id) DO UPDATE SET enabled = excluded.enabled, group_ids = excluded.group_ids, config = excluded.config,
				capture_enabled = excluded.capture_enabled, capture_sample_rate = excluded.capture_sample_rate, updated_at = CURRENT_TIMESTAMP`,
			state.ID, state.Enabled, string(groups), string(state.Config), state.CaptureEnabled, state.CaptureSampleRate); err != nil {
			return err
		}
		return insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityPlugin, 0, "updated")
	})
}

// SetAccountTransportPluginOverride writes (enabled != nil) or clears
// (enabled == nil, stored as JSON null) an account's per-plugin override
// credential. The account outbox triggers only watch routing credential keys,
// so an explicit account event is appended for the other replicas.
func (db *DB) SetAccountTransportPluginOverride(ctx context.Context, accountID int64, credentialKey string, enabled *bool) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database is not initialized")
	}
	if accountID <= 0 || !sqliteJSONSetKeySupported(credentialKey) {
		return fmt.Errorf("invalid transport plugin override")
	}
	var value any
	if enabled != nil {
		value = *enabled
	}
	if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{credentialKey: value}); err != nil {
		return err
	}
	return db.InsertSchedulerOutboxEvent(ctx, SchedulerEntityAccount, accountID, "updated")
}

// normalizeUsageLogTransport maps "" to native and clamps unexpected values so
// a bad plugin ID can never wedge a usage-log batch on the VARCHAR(32) column.
func normalizeUsageLogTransport(transport string) string {
	transport = strings.TrimSpace(transport)
	if transport == "" {
		return TransportNative
	}
	return clampUsageLogText(transport, 32)
}
