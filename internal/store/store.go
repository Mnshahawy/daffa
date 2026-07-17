// Package store persists Daffa's state in either SQLite or an existing PostgreSQL.
//
// The schema is deliberately written in the common subset of both dialects (TEXT,
// INTEGER, no dialect-specific types; timestamps are RFC3339 TEXT) so feature code
// never has to care which one it is talking to. Anything that cannot be expressed
// portably belongs behind this package, not in a handler.
//
// Daffa never provisions a Postgres server. Given a postgres:// URL it expects the
// role and database to exist, and confines itself to its own schema so it can share
// a cluster with anything else.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

var ErrNotFound = errors.New("store: not found")

// neverMatches is what envIn returns when the caller may see no host at all. It is a
// sentinel rather than an empty string precisely so it cannot be mistaken for "no filter" —
// which is the difference between showing nothing and showing everything.
const neverMatches = "\x00never"

// envIn builds the `WHERE env_id IN (…)` clause for a caller's visible hosts.
//
// global short-circuits to no clause (see everything). Otherwise the clause names exactly
// the hosts they hold the relevant capability on. An empty list yields neverMatches — the
// caller must return no rows rather than fall through to an unfiltered query, which is the
// classic way an authorization filter turns into an authorization hole.
func envIn(global bool, envs []string) (string, []any) {
	if global {
		return "", nil
	}
	if len(envs) == 0 {
		return neverMatches, nil
	}
	ph := make([]string, len(envs))
	args := make([]any, len(envs))
	for i, e := range envs {
		ph[i] = "?"
		args[i] = e
	}
	return " WHERE env_id IN (" + strings.Join(ph, ",") + ")", args
}

// IsDuplicate reports whether an error is a unique-constraint violation, in either
// dialect. Both drivers report it only in the message text — SQLite says "UNIQUE
// constraint failed", Postgres says "duplicate key value violates unique constraint" — so
// string matching is what there is. Keeping it here means the two spellings are written
// down once, rather than at every call site that wants to turn a collision into a 409.
func IsDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "duplicate key")
}

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type Store struct {
	db       *sql.DB
	dialect  Dialect
	pgSchema string // Postgres only: the schema Daffa confines itself to
	// SQLite serializes writes through one connection; Postgres uses the pool.
	writeMu chan struct{}
}

// Open connects to the store described by dbURL and applies migrations.
func Open(ctx context.Context, dbURL string) (*Store, error) {
	u, err := url.Parse(dbURL)
	if err != nil {
		return nil, fmt.Errorf("store: parsing DAFFA_DB_URL: %w", err)
	}

	var s *Store
	switch {
	case u.Scheme == "sqlite" || u.Scheme == "file":
		s, err = openSQLite(dbURL)
	case u.Scheme == "postgres" || u.Scheme == "postgresql":
		s, err = openPostgres(dbURL)
	default:
		return nil, fmt.Errorf("store: unsupported DAFFA_DB_URL scheme %q (want sqlite:// or postgres://)", u.Scheme)
	}
	if err != nil {
		return nil, err
	}

	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}

	// The sample table is partitioned and its DDL is dialect-specific, so it is not in a
	// migration — see the note in migrate.go's 0012, and metrics.go.
	if err := s.InitMetrics(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error     { return s.db.Close() }
func (s *Store) Dialect() Dialect { return s.dialect }
func (s *Store) DB() *sql.DB      { return s.db }

// lockWrites serializes writers on SQLite, where concurrent writes would other-
// wise surface as SQLITE_BUSY. On Postgres it is a no-op.
func (s *Store) lockWrites() func() {
	if s.writeMu == nil {
		return func() {}
	}
	s.writeMu <- struct{}{}
	return func() { <-s.writeMu }
}

// rebind rewrites the portable `?` placeholders into $N for Postgres.
func (s *Store) rebind(query string) string {
	if s.dialect != Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$")
			b.WriteString(fmt.Sprint(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	defer s.lockWrites()()
	return s.db.ExecContext(ctx, s.rebind(query), args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(query), args...)
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(query), args...)
}

// Timestamps are stored as RFC3339 UTC text in both dialects — portable, sortable,
// and human-readable in a psql session or a sqlite3 shell.
//
// Caveat, deliberately accepted: RFC3339Nano TRIMS trailing fractional zeros, so text order is
// not chronological order WITHIN a second. "…05Z" (a whole second) sorts after "…05.5Z" because
// 'Z' (0x5A) > '.' (0x2E), even though it is earlier. So an `ORDER BY started_at` list or an
// `expires_at < ?` check can be off by sub-second amounts when two rows share a second. That is
// negligible for the audit/deployment/session/outbox tables this formats — but it is exactly the
// trap that matters for high-rate samples, which is why metric_samples stores epoch INTEGERS
// instead and does not come through here. See docs/monitoring.md. Do not switch this to a
// fixed-width fractional format to "fix" it: existing rows are already trimmed, so a padded write
// would only sort inconsistently against them across the changeover — it needs a data migration,
// not a format tweak, and the payoff does not justify one.
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(v string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}
