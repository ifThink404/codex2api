package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProxyLocation is the approximate egress location recorded by the last
// successful proxy test. It lives on the proxies row next to test_timezone and
// is never resolved on the request path.
type ProxyLocation struct {
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	City     string `json:"city,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// ProxyTestGeo carries the structured fields of a proxy test result.
type ProxyTestGeo struct {
	CountryCode string
	Region      string
	City        string
}

// NormalizeProxyCountryCode accepts only an ISO 3166-1 alpha-2 code.
func NormalizeProxyCountryCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 2 || value[0] < 'A' || value[0] > 'Z' || value[1] < 'A' || value[1] > 'Z' {
		return ""
	}
	return value
}

// NormalizeProxyLocationText bounds a region/city name and rejects control characters.
func NormalizeProxyLocationText(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > 128 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

// Location returns the row's normalized stored egress location.
func (p *ProxyRow) Location() ProxyLocation {
	if p == nil {
		return ProxyLocation{}
	}
	return ProxyLocation{
		Country:  NormalizeProxyCountryCode(p.TestCountryCode),
		Region:   NormalizeProxyLocationText(p.TestRegion),
		City:     NormalizeProxyLocationText(p.TestCity),
		Timezone: strings.TrimSpace(p.TestTimezone),
	}
}

func (db *DB) ensureProxyLocation(ctx context.Context) error {
	if db.isSQLite() {
		for table, columns := range map[string][][2]string{
			"proxies": {
				{"test_country_code", "TEXT DEFAULT ''"},
				{"test_region", "TEXT DEFAULT ''"},
				{"test_city", "TEXT DEFAULT ''"},
			},
			"system_settings": {{"codex_web_search_proxy_location", "INTEGER NOT NULL DEFAULT 0"}},
		} {
			existing, err := db.sqliteTableColumns(ctx, table)
			if err != nil {
				return err
			}
			for _, column := range columns {
				if _, ok := existing[column[0]]; ok {
					continue
				}
				if _, err := db.conn.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column[0]+` `+column[1]); err != nil {
					return err
				}
			}
		}
	} else {
		for _, statement := range []string{
			`ALTER TABLE proxies ADD COLUMN IF NOT EXISTS test_country_code VARCHAR(2) DEFAULT ''`,
			`ALTER TABLE proxies ADD COLUMN IF NOT EXISTS test_region VARCHAR(128) DEFAULT ''`,
			`ALTER TABLE proxies ADD COLUMN IF NOT EXISTS test_city VARCHAR(128) DEFAULT ''`,
			`ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_web_search_proxy_location BOOLEAN NOT NULL DEFAULT FALSE`,
		} {
			if _, err := db.conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
	}
	var enabled bool
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(codex_web_search_proxy_location, `+db.sqlBool(false)+`) FROM system_settings WHERE id=1`).Scan(&enabled)
	if err == nil {
		db.codexWebSearchProxyLocation.Store(enabled)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// GetCodexWebSearchProxyLocation reports the persisted switch loaded at startup
// or by the last successful save (default off).
func (db *DB) GetCodexWebSearchProxyLocation() bool {
	return db != nil && db.codexWebSearchProxyLocation.Load()
}

// SaveCodexWebSearchProxyLocation persists the switch outside the monolithic
// system settings upsert and updates the in-process copy on success.
func (db *DB) SaveCodexWebSearchProxyLocation(ctx context.Context, enabled bool) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database is not initialized")
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id, codex_web_search_proxy_location) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET codex_web_search_proxy_location = EXCLUDED.codex_web_search_proxy_location`, enabled); err != nil {
		return err
	}
	db.codexWebSearchProxyLocation.Store(enabled)
	return nil
}
