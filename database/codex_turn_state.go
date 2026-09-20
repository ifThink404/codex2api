package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const CodexTurnStateTTL = 7 * 24 * time.Hour

type TurnStateEvent struct {
	Action         string     `json:"action"`
	Carrier        string     `json:"carrier"`
	Alias          string     `json:"alias,omitempty"`
	RealHash       string     `json:"real_hash,omitempty"`
	Received       string     `json:"received,omitempty"`
	Real           string     `json:"real,omitempty"`
	ValueTruncated bool       `json:"value_truncated,omitempty"`
	AccountID      int64      `json:"account_id,omitempty"`
	Generation     uint64     `json:"generation"`
	At             time.Time  `json:"at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
}

type TurnStateDiagnostic struct {
	ScopeHash string           `json:"scope_hash"`
	Events    []TurnStateEvent `json:"events"`
	Omitted   int              `json:"omitted,omitempty"`
}

// The mapping stores the upstream token encrypted. Explicit admin diagnostics
// separately retain the values needed to compare ingress, upstream and aliases.
type CodexTurnStateBinding struct {
	Scope       string `json:"scope"`
	RootKey     string `json:"root_key"`
	AccountID   int64  `json:"account_id"`
	AccountHash string `json:"account_hash"`
	Generation  uint64 `json:"generation"`
}

type CodexTurnStateRecord struct {
	CodexTurnStateBinding
	Alias     string    `json:"alias"`
	Real      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ValidCodexTurnStateAlias(value string) bool {
	_, ok := codexTurnStateEnvelope(value)
	return ok // Shape only; possession of this format never authorizes restoration.
}

func codexTurnStateEnvelope(value string) ([]byte, bool) {
	if len(value) > 16384 {
		return nil, false
	}
	b, err := base64.URLEncoding.DecodeString(value)
	return b, err == nil && len(b) >= 73 && b[0] == 0x80 && (len(b)-57)%16 == 0 && base64.URLEncoding.EncodeToString(b) == value
}

func (db *DB) turnStateAliasSignature(payload []byte) []byte {
	mac := hmac.New(sha256.New, db.turnStateKey)
	mac.Write([]byte("codex-turn-state-alias-v2\x00"))
	mac.Write(payload)
	return mac.Sum(nil)
}

// Local authentication distinguishes our opaque handles from identically
// shaped official values without relying on a visible custom prefix.
func (db *DB) IsManagedCodexTurnStateAlias(value string) bool {
	if db == nil || len(db.turnStateKey) != 32 {
		return false
	}
	b, ok := codexTurnStateEnvelope(value)
	return ok && hmac.Equal(b[len(b)-32:], db.turnStateAliasSignature(b[:len(b)-32]))
}

func (db *DB) newCodexTurnStateAlias(real string) (string, error) {
	template, matches := codexTurnStateEnvelope(real)
	size := 217 // Same layout and length as the observed 292-character token.
	if matches {
		size = len(template)
	}
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[0] = 0x80
	binary.BigEndian.PutUint64(b[1:9], uint64(time.Now().Unix()))
	if matches {
		copy(b[:9], template[:9])
	} // Preserve the public version/time shape only.
	copy(b[size-32:], db.turnStateAliasSignature(b[:size-32]))
	return base64.URLEncoding.EncodeToString(b), nil
}

func (db *DB) ensureCodexTurnStateTable(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS codex_turn_state_secret (id INTEGER PRIMARY KEY, secret TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_turn_states (alias TEXT PRIMARY KEY, source_key TEXT NOT NULL UNIQUE, binding TEXT NOT NULL, ciphertext TEXT NOT NULL, created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_turn_states_expiry ON codex_turn_states(expires_at)`,
		`CREATE TABLE IF NOT EXISTS codex_response_ids (alias TEXT PRIMARY KEY, source_key TEXT NOT NULL UNIQUE, binding TEXT NOT NULL, ciphertext TEXT NOT NULL, expires_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_response_ids_expiry ON codex_response_ids(expires_at)`,
		`CREATE TABLE IF NOT EXISTS codex_protocol_ids (public_key TEXT PRIMARY KEY, upstream_key TEXT NOT NULL UNIQUE, ciphertext TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_project_registry (source_key TEXT PRIMARY KEY)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var secret string
		err := tx.QueryRowContext(ctx, `SELECT secret FROM codex_turn_state_secret WHERE id=1`).Scan(&secret)
		if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM codex_turn_states) + (SELECT COUNT(*) FROM codex_response_ids) + (SELECT COUNT(*) FROM codex_protocol_ids) + (SELECT COUNT(*) FROM codex_project_registry)`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("turn-state encryption key missing")
			}
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO codex_turn_state_secret(id,secret) VALUES(1,$1) ON CONFLICT(id) DO NOTHING`, hex.EncodeToString(key)); err != nil {
				return err
			}
			err = tx.QueryRowContext(ctx, `SELECT secret FROM codex_turn_state_secret WHERE id=1`).Scan(&secret)
		}
		if err != nil {
			return err
		}
		key, err := hex.DecodeString(secret)
		if err != nil || len(key) != 32 {
			return errors.New("invalid turn-state encryption key")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return err
		}
		db.turnStateCipher, err = cipher.NewGCM(block)
		db.turnStateKey = key
		return err
	})
}

func (db *DB) IssueCodexTurnState(ctx context.Context, binding CodexTurnStateBinding, real string) (CodexTurnStateRecord, error) {
	var result CodexTurnStateRecord
	if db.turnStateCipher == nil || binding.Scope == "" || binding.AccountID <= 0 || real == "" || len(real) > 16384 {
		return result, errors.New("invalid turn-state mapping")
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return result, err
	}
	mac := hmac.New(sha256.New, db.turnStateKey)
	mac.Write([]byte("codex-turn-state-source-v2\x00"))
	mac.Write(encoded)
	mac.Write([]byte{0})
	mac.Write([]byte(real))
	source := hex.EncodeToString(mac.Sum(nil))
	alias, err := db.newCodexTurnStateAlias(real)
	if err != nil {
		return result, err
	}
	nonce := make([]byte, db.turnStateCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return result, err
	}
	sealed := db.turnStateCipher.Seal(nonce, nonce, []byte(real), []byte(alias+"\x00"+string(encoded)))
	now := time.Now().UTC()
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		// Bound cleanup work; index-backed batches avoid full-table scans on request paths.
		if db.turnStateWrites.Add(1)%256 == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM codex_turn_states WHERE alias IN (SELECT alias FROM codex_turn_states WHERE expires_at <= $1 ORDER BY expires_at LIMIT 512)`, now.Unix()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM codex_turn_states WHERE source_key=$1 AND expires_at <= $2`, source, now.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO codex_turn_states(alias,source_key,binding,ciphertext,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(source_key) DO UPDATE SET expires_at=excluded.expires_at`, alias, source, string(encoded), base64.StdEncoding.EncodeToString(sealed), now.Unix(), now.Add(CodexTurnStateTTL).Unix())
		if err != nil {
			return err
		}
		var created, expires int64
		if err := tx.QueryRowContext(ctx, `SELECT alias,created_at,expires_at FROM codex_turn_states WHERE source_key=$1`, source).Scan(&result.Alias, &created, &expires); err != nil {
			return err
		}
		result.CodexTurnStateBinding, result.Real = binding, real
		result.CreatedAt, result.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
		return nil
	})
	return result, err
}

func (db *DB) ReadCodexTurnState(ctx context.Context, alias string) (CodexTurnStateRecord, bool, error) {
	var result CodexTurnStateRecord
	if !db.IsManagedCodexTurnStateAlias(alias) {
		return result, false, nil
	}
	var binding, ciphertext string
	var created, expires int64
	err := db.conn.QueryRowContext(ctx, `SELECT binding,ciphertext,created_at,expires_at FROM codex_turn_states WHERE alias=$1 AND expires_at > $2`, alias, time.Now().Unix()).Scan(&binding, &ciphertext, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if json.Unmarshal([]byte(binding), &result.CodexTurnStateBinding) != nil || db.turnStateCipher == nil {
		return result, false, errors.New("invalid turn-state mapping")
	}
	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil || len(sealed) < db.turnStateCipher.NonceSize() {
		return result, false, errors.New("invalid turn-state ciphertext")
	}
	n := db.turnStateCipher.NonceSize()
	real, err := db.turnStateCipher.Open(nil, sealed[:n], sealed[n:], []byte(alias+"\x00"+binding))
	if err != nil {
		return result, false, fmt.Errorf("turn-state decryption failed: %w", err)
	}
	result.Alias, result.Real = alias, string(real)
	result.CreatedAt, result.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
	return result, true, nil
}
