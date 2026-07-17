package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func openSQLite(dbURL string) (*Store, error) {
	path := strings.TrimPrefix(strings.TrimPrefix(dbURL, "sqlite://"), "file://")
	if path == "" {
		return nil, fmt.Errorf("store: sqlite URL has no path: %q", dbURL)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: creating sqlite dir: %w", err)
		}
	}

	// WAL for reader/writer concurrency; busy_timeout so the rare contended write
	// waits instead of failing; foreign_keys because we rely on cascades.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)

	return &Store{db: db, dialect: SQLite, writeMu: make(chan struct{}, 1)}, nil
}

func openPostgres(dbURL string) (*Store, error) {
	// Daffa keeps its tables in their own schema so it can live in a shared
	// cluster (e.g. alongside other apps' databases) without colliding.
	u, err := url.Parse(dbURL)
	if err != nil {
		return nil, fmt.Errorf("store: parsing postgres URL: %w", err)
	}
	q := u.Query()
	schema := q.Get("search_path")
	if schema == "" {
		schema = "daffa"
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
	}

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: opening postgres: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)

	s := &Store{db: db, dialect: Postgres, pgSchema: schema}
	return s, nil
}
