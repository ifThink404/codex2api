package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// session_auto_locks：连续最终 500 达阈值后自动锁定的会话。键是网关的会话粘性键
// （会话 ID + API Key），锁没有 TTL，只有管理员手动解锁。
type SessionAutoLock struct {
	ID              int64     `json:"id"`
	SessionKey      string    `json:"session_key"`
	SessionIDPrefix string    `json:"session_id_prefix"`
	APIKeyID        int64     `json:"api_key_id"`
	AccountID       int64     `json:"account_id"`
	ErrorMessage    string    `json:"error_message"`
	Threshold       int       `json:"threshold"`
	Source          string    `json:"source"`
	LockedAt        time.Time `json:"locked_at"`
	CreatedAt       time.Time `json:"created_at"`
}

type SessionAutoLockInput struct {
	SessionKey      string
	SessionIDPrefix string
	APIKeyID        int64
	AccountID       int64
	ErrorMessage    string
	Threshold       int
}

var sessionAutoLockSchemaMu sync.Mutex

func (db *DB) ensureSessionAutoLocksTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	sessionAutoLockSchemaMu.Lock()
	defer sessionAutoLockSchemaMu.Unlock()
	idType, timeType := "BIGSERIAL PRIMARY KEY", "TIMESTAMPTZ"
	if db.isSQLite() {
		idType, timeType = "INTEGER PRIMARY KEY AUTOINCREMENT", "TIMESTAMP"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS session_auto_locks (
		id %s,
		session_key VARCHAR(255) NOT NULL UNIQUE,
		session_id_prefix VARCHAR(32) NOT NULL DEFAULT '',
		api_key_id BIGINT NOT NULL DEFAULT 0,
		account_id BIGINT NOT NULL DEFAULT 0,
		error_message VARCHAR(255) NOT NULL DEFAULT '',
		threshold INT NOT NULL DEFAULT 3,
		source VARCHAR(24) NOT NULL DEFAULT 'automatic',
		locked_at %s NOT NULL,
		created_at %s NOT NULL
	)`, idType, timeType, timeType)
	for _, statement := range []string{ddl, `CREATE INDEX IF NOT EXISTS idx_session_auto_locks_locked_at ON session_auto_locks(locked_at)`} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

const sessionAutoLockSelect = `SELECT id, session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at FROM session_auto_locks`

func scanSessionAutoLock(scanner interface{ Scan(...any) error }) (*SessionAutoLock, error) {
	var item SessionAutoLock
	if err := scanner.Scan(&item.ID, &item.SessionKey, &item.SessionIDPrefix, &item.APIKeyID, &item.AccountID, &item.ErrorMessage, &item.Threshold, &item.Source, &item.LockedAt, &item.CreatedAt); err != nil {
		return nil, err
	}
	return &item, nil
}

func clampSessionAutoLockText(value string, max int) string {
	return clampUsageLogText(strings.TrimSpace(value), max)
}

// InsertSessionAutoLock 按 session_key 幂等：已存在时返回现有行且 created=false。
func (db *DB) InsertSessionAutoLock(ctx context.Context, input SessionAutoLockInput) (*SessionAutoLock, bool, error) {
	if db == nil {
		return nil, false, errors.New("database unavailable")
	}
	key := clampSessionAutoLockText(input.SessionKey, 255)
	if key == "" {
		return nil, false, errors.New("session key required")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, false, err
	}
	if existing, err := db.getSessionAutoLockByKey(ctx, key); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, false, nil
	}
	now := time.Now().UTC()
	query := `INSERT INTO session_auto_locks (session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, 'automatic', $7, $8)`
	if db.isSQLite() {
		query = `INSERT INTO session_auto_locks (session_key, session_id_prefix, api_key_id, account_id, error_message, threshold, source, locked_at, created_at) VALUES (?, ?, ?, ?, ?, ?, 'automatic', ?, ?)`
	}
	if _, err := db.conn.ExecContext(ctx, query, key, clampSessionAutoLockText(input.SessionIDPrefix, 32), input.APIKeyID, input.AccountID, clampSessionAutoLockText(input.ErrorMessage, 255), NormalizeSessionAutoLockThreshold(input.Threshold), now, now); err != nil {
		// 并发写同一键：另一路已插入，回读即可。
		if existing, lookupErr := db.getSessionAutoLockByKey(ctx, key); lookupErr == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}
	created, err := db.getSessionAutoLockByKey(ctx, key)
	if err != nil {
		return nil, false, err
	}
	return created, true, nil
}

func (db *DB) getSessionAutoLockByKey(ctx context.Context, key string) (*SessionAutoLock, error) {
	query := sessionAutoLockSelect + ` WHERE session_key = $1`
	if db.isSQLite() {
		query = sessionAutoLockSelect + ` WHERE session_key = ?`
	}
	item, err := scanSessionAutoLock(db.conn.QueryRowContext(ctx, query, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

func (db *DB) ListSessionAutoLocks(ctx context.Context, limit int) ([]SessionAutoLock, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := sessionAutoLockSelect + ` ORDER BY locked_at DESC, id DESC LIMIT $1`
	if db.isSQLite() {
		query = sessionAutoLockSelect + ` ORDER BY locked_at DESC, id DESC LIMIT ?`
	}
	rows, err := db.conn.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionAutoLock, 0)
	for rows.Next() {
		item, err := scanSessionAutoLock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

// ListSessionAutoLockKeys 供进程启动时预热内存锁表。
func (db *DB) ListSessionAutoLockKeys(ctx context.Context) ([]string, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT session_key FROM session_auto_locks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// DeleteSessionAutoLock 返回被删除的行；不存在返回 nil, nil。
func (db *DB) DeleteSessionAutoLock(ctx context.Context, id int64) (*SessionAutoLock, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	if err := db.ensureSessionAutoLocksTable(ctx); err != nil {
		return nil, err
	}
	query := sessionAutoLockSelect + ` WHERE id = $1`
	del := `DELETE FROM session_auto_locks WHERE id = $1`
	if db.isSQLite() {
		query = sessionAutoLockSelect + ` WHERE id = ?`
		del = `DELETE FROM session_auto_locks WHERE id = ?`
	}
	item, err := scanSessionAutoLock(db.conn.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := db.conn.ExecContext(ctx, del, id); err != nil {
		return nil, err
	}
	return item, nil
}
