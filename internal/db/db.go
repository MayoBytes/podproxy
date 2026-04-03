package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS feeds (
    id                       TEXT PRIMARY KEY,
    title                    TEXT,
    original_url             TEXT NOT NULL,
    last_fetched_at          DATETIME,
    refresh_interval_minutes INT DEFAULT 60,
    auto_prefetch            BOOLEAN DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS episodes (
    id           TEXT PRIMARY KEY,
    feed_id      TEXT NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
    title        TEXT,
    original_url TEXT NOT NULL,
    cached_path  TEXT,
    pub_date     DATETIME,
    duration_sec INT,
    size_bytes   INT,
    cache_status TEXT DEFAULT 'none',
    url_id       TEXT NOT NULL,
    UNIQUE(feed_id, url_id)
);

CREATE INDEX IF NOT EXISTS idx_episodes_feed_id ON episodes(feed_id);
`

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "podproxy.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	sqlDB.SetMaxOpenConns(1)

	pragmas := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
	}
	for _, p := range pragmas {
		if _, err := sqlDB.Exec(p); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("set pragma %q: %w", p, err)
		}
	}

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &DB{sqlDB}, nil
}
