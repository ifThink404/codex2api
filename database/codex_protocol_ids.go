package database

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// Functional protocol handles have a separate authenticated namespace. Both
// directions are encrypted; indexes contain only keyed hashes. They have the
// same lifetime as persisted identity epochs (history can outlive response IDs).
type CodexProtocolPair struct {
	Public   string
	Upstream string
}

func (db *DB) protocolKey(binding CodexTurnStateBinding, kind, direction, value string) string {
	b, _ := json.Marshal(binding)
	return hex.EncodeToString(db.responseIDMAC("protocol-"+kind+"-"+direction, append(append(b, 0), []byte(value)...)))
}

func (db *DB) ReadCodexProtocolPair(ctx context.Context, binding CodexTurnStateBinding, kind, value string, public bool) (CodexProtocolPair, bool, error) {
	var pair CodexProtocolPair
	column, direction := "upstream_key", "upstream"
	if public {
		column, direction = "public_key", "public"
	}
	var publicKey, upstreamKey, ciphertext string
	err := db.conn.QueryRowContext(ctx, `SELECT public_key,upstream_key,ciphertext FROM codex_protocol_ids WHERE `+column+`=$1`, db.protocolKey(binding, kind, direction, value)).Scan(&publicKey, &upstreamKey, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return pair, false, nil
	}
	if err != nil {
		return pair, false, err
	}
	if db.turnStateCipher == nil {
		return pair, false, errors.New("protocol identity encryption unavailable")
	}
	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	n := db.turnStateCipher.NonceSize()
	if err != nil || len(sealed) < n {
		return pair, false, errors.New("invalid protocol identity ciphertext")
	}
	raw, err := db.turnStateCipher.Open(nil, sealed[:n], sealed[n:], []byte("protocol-id-v1\x00"+publicKey+"\x00"+upstreamKey))
	if err != nil {
		return pair, false, err
	}
	err = json.Unmarshal(raw, &pair)
	return pair, err == nil, err
}

func (db *DB) PutCodexProtocolPair(ctx context.Context, binding CodexTurnStateBinding, kind string, pair CodexProtocolPair) error {
	if db.turnStateCipher == nil || binding.Scope == "" || binding.RootKey == "" || binding.AccountID <= 0 || (kind != "turn" && kind != "conversation" && kind != "project") || pair.Public == "" || pair.Upstream == "" || len(pair.Public) > 256 || len(pair.Upstream) > 256 || pair.Public == pair.Upstream {
		return errors.New("invalid protocol identity mapping")
	}
	if existing, found, err := db.ReadCodexProtocolPair(ctx, binding, kind, pair.Public, true); err != nil {
		return err
	} else if found {
		if existing == pair {
			return nil
		}
		return ErrCodexIdentityAliasCollision
	}
	publicKey := db.protocolKey(binding, kind, "public", pair.Public)
	upstreamKey := db.protocolKey(binding, kind, "upstream", pair.Upstream)
	nonce := make([]byte, db.turnStateCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	raw, _ := json.Marshal(pair)
	sealed := db.turnStateCipher.Seal(nonce, nonce, raw, []byte("protocol-id-v1\x00"+publicKey+"\x00"+upstreamKey))
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO codex_protocol_ids(public_key,upstream_key,ciphertext) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, publicKey, upstreamKey, base64.StdEncoding.EncodeToString(sealed))
		return err
	})
	if err != nil {
		return err
	}
	existing, found, err := db.ReadCodexProtocolPair(ctx, binding, kind, pair.Public, true)
	if err != nil {
		return err
	}
	if !found || existing != pair {
		return ErrCodexIdentityAliasCollision
	}
	return nil
}

// Deterministic but domain/scoped aliases prevent cross-replica races. The
// signature identifies our namespace, never authorizes another owner's handle.
func (db *DB) CodexConversationAlias(binding CodexTurnStateBinding, real string) string {
	key := db.protocolKey(binding, "conversation", "alias", real)
	random, _ := hex.DecodeString(key[:32])
	return "conv_" + key[:32] + hex.EncodeToString(db.responseIDMAC("conversation-alias", random)[:16])
}

func (db *DB) IsManagedCodexConversationAlias(value string) bool {
	if len(value) != 69 || !strings.HasPrefix(value, "conv_") {
		return false
	}
	b, err := hex.DecodeString(value[5:])
	return err == nil && hmac.Equal(db.responseIDMAC("conversation-alias", b[:16])[:16], b[16:])
}
