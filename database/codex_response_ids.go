package database

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Response handles use a separate namespace/AAD from turn-state tokens. The
// persisted encryption key is shared, so replicas and restarts resolve the same
// handles. Scope is the downstream owner; RootKey binds the original session.
type CodexResponseIDRecord struct {
	CodexTurnStateBinding
	Alias     string    `json:"alias"`
	Real      string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
}

const CodexResponseIDTTL = 7 * 24 * time.Hour

// Only exposed by administrator request diagnostics, never by public responses.
type ResponseIdentityEvent struct {
	Action        string `json:"action"`
	Received      string `json:"received,omitempty"`
	Alias         string `json:"alias,omitempty"`
	Original      string `json:"original,omitempty"`
	OriginalError string `json:"original_error,omitempty"`
	AccountID     int64  `json:"account_id,omitempty"`
	Generation    uint64 `json:"generation"`
}

// Safe local mapping diagnostics; raw driver messages and handle values are
// deliberately excluded from both usage logs and service-error exports.
type ResponseMappingFailure struct {
	Operation  string `json:"operation"`
	Reason     string `json:"reason"`
	SQLState   string `json:"sql_state,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

func (db *DB) responseIDMAC(domain string, value []byte) []byte {
	mac := hmac.New(sha256.New, db.turnStateKey)
	mac.Write([]byte("codex-response-id-" + domain + "-v1\x00"))
	mac.Write(value)
	return mac.Sum(nil)
}

func (db *DB) IsManagedCodexResponseID(value string) bool {
	if db == nil || len(db.turnStateKey) != 32 || len(value) != 69 || !strings.HasPrefix(value, "resp_") {
		return false
	}
	b, err := hex.DecodeString(value[5:])
	return err == nil && hmac.Equal(b[16:], db.responseIDMAC("alias", b[:16])[:16])
}

func (db *DB) IssueCodexResponseID(ctx context.Context, binding CodexTurnStateBinding, real string) (CodexResponseIDRecord, error) {
	var result CodexResponseIDRecord
	if db.turnStateCipher == nil || binding.Scope == "" || binding.RootKey == "" || binding.AccountID <= 0 || real == "" || len(real) > 256 {
		return result, errors.New("invalid response ID mapping")
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return result, err
	}
	source := hex.EncodeToString(db.responseIDMAC("source", append(append(encoded[:len(encoded):len(encoded)], 0), []byte(real)...)))
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return result, err
	}
	alias := "resp_" + hex.EncodeToString(append(random, db.responseIDMAC("alias", random)[:16]...))
	nonce := make([]byte, db.turnStateCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return result, err
	}
	sealed := db.turnStateCipher.Seal(nonce, nonce, []byte(real), []byte("response-id-v1\x00"+alias+"\x00"+string(encoded)))
	now := time.Now().UTC()
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		// Indexed and bounded; never scan an entire history on the request path.
		if db.responseIDWrites.Add(1)%256 == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM codex_response_ids WHERE alias IN (SELECT alias FROM codex_response_ids WHERE expires_at <= $1 ORDER BY expires_at LIMIT 512)`, now.Unix()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM codex_response_ids WHERE source_key=$1 AND expires_at <= $2`, source, now.Unix()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_response_ids(alias,source_key,binding,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(source_key) DO UPDATE SET expires_at=excluded.expires_at`, alias, source, string(encoded), base64.StdEncoding.EncodeToString(sealed), now.Add(CodexResponseIDTTL).Unix()); err != nil {
			return err
		}
		var expires int64
		if err := tx.QueryRowContext(ctx, `SELECT alias,expires_at FROM codex_response_ids WHERE source_key=$1`, source).Scan(&result.Alias, &expires); err != nil {
			return err
		}
		result.CodexTurnStateBinding, result.Real, result.ExpiresAt = binding, real, time.Unix(expires, 0).UTC()
		return nil
	})
	return result, err
}

func (db *DB) ReadCodexResponseID(ctx context.Context, alias string) (CodexResponseIDRecord, bool, error) {
	var result CodexResponseIDRecord
	if !db.IsManagedCodexResponseID(alias) {
		return result, false, nil
	}
	var binding, ciphertext string
	var expires int64
	err := db.conn.QueryRowContext(ctx, `SELECT binding,ciphertext,expires_at FROM codex_response_ids WHERE alias=$1 AND expires_at > $2`, alias, time.Now().Unix()).Scan(&binding, &ciphertext, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if db.turnStateCipher == nil || json.Unmarshal([]byte(binding), &result.CodexTurnStateBinding) != nil {
		return result, false, errors.New("invalid response ID binding")
	}
	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	n := db.turnStateCipher.NonceSize()
	if err != nil || len(sealed) < n {
		return result, false, errors.New("invalid response ID ciphertext")
	}
	real, err := db.turnStateCipher.Open(nil, sealed[:n], sealed[n:], []byte("response-id-v1\x00"+alias+"\x00"+binding))
	if err != nil {
		return result, false, err
	}
	result.Alias, result.Real, result.ExpiresAt = alias, string(real), time.Unix(expires, 0).UTC()
	return result, true, nil
}
