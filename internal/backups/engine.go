// Package backups dumps databases out of running containers and streams them to object
// storage.
//
// The shape is a pipe, end to end:
//
//	docker exec pg_dumpall → gzip → age (optional) → S3 multipart upload
//
// Nothing is buffered to disk at either end, and memory stays constant regardless of
// whether the database is 10 MB or 100 GB. That is not an optimization; it is the only
// way this works on a small box whose disk is already mostly full of the database you
// are trying to back up.
package backups

import (
	"fmt"
	"strings"
)

type Engine string

const (
	Postgres Engine = "postgres"
	MySQL    Engine = "mysql"
	MongoDB  Engine = "mongodb"
	// Volume is the file-level engine: a tar of a named volume, for file-shaped data with
	// no dump tool (forgejo repositories, uploads). It never execs into anything —
	// see RunVolume — so dumpCommand/restoreCommand refuse it by construction.
	Volume Engine = "volume"
)

// ValidEngine reports whether e is a DATABASE engine — one with a dump command to exec.
// The volume engine is validated separately by its handler, because everything about its
// job shape (a volume, not a container) differs.
func ValidEngine(e Engine) bool {
	switch e {
	case Postgres, MySQL, MongoDB:
		return true
	}
	return false
}

// Spec is everything the pipeline needs to know about WHAT to dump.
type Spec struct {
	Engine    Engine
	Databases string // space or comma separated; empty = everything
	User      string
	Password  string
}

// dumpCommand is what gets exec'd inside the database container.
//
// It relies on the dump tools being present in that container — which they are, in every
// official postgres/mysql/mongo image, because the image that runs the server also ships
// its client tools. That constraint is documented rather than worked around: the
// alternative (a sidecar with its own client, networked to the database) means a second
// container, a second set of credentials, and a version skew waiting to happen.
func (s Spec) dumpCommand() ([]string, error) {
	dbs := splitDatabases(s.Databases)

	switch s.Engine {
	case Postgres:
		user := s.User
		if user == "" {
			user = "postgres"
		}
		if len(dbs) == 0 {
			// The whole cluster, including roles and grants. A per-database dump that
			// silently loses the roles is a restore that fails at 3am.
			return []string{"pg_dumpall", "-U", user, "--clean", "--if-exists"}, nil
		}
		if len(dbs) > 1 {
			return nil, fmt.Errorf("backups: postgres can dump one database at a time — leave the field empty to dump the whole cluster")
		}
		return []string{"pg_dump", "-U", user, "--clean", "--if-exists", dbs[0]}, nil

	case MySQL:
		args := []string{"mysqldump", "--single-transaction", "--quick", "--routines", "--triggers"}
		if s.User != "" {
			args = append(args, "-u", s.User)
		}
		if s.Password != "" {
			// Passed inline because exec gives us no other channel; it is visible only
			// inside that container's process list, for the seconds the dump runs.
			args = append(args, "-p"+s.Password)
		}
		if len(dbs) == 0 {
			args = append(args, "--all-databases")
		} else {
			args = append(args, "--databases")
			args = append(args, dbs...)
		}
		return args, nil

	case MongoDB:
		// --archive writes a single stream to stdout, which is exactly what we want.
		args := []string{"mongodump", "--archive"}
		if s.User != "" {
			args = append(args, "-u", s.User, "--authenticationDatabase", "admin")
		}
		if s.Password != "" {
			args = append(args, "-p", s.Password)
		}
		for _, db := range dbs {
			args = append(args, "--db", db)
		}
		return args, nil

	default:
		return nil, fmt.Errorf("backups: unknown engine %q", s.Engine)
	}
}

// restoreCommand reads the dump back in from stdin.
func (s Spec) restoreCommand() ([]string, error) {
	dbs := splitDatabases(s.Databases)

	switch s.Engine {
	case Postgres:
		user := s.User
		if user == "" {
			user = "postgres"
		}
		// psql, not pg_restore: a pg_dumpall/pg_dump --clean dump is plain SQL.
		args := []string{"psql", "-U", user, "--set", "ON_ERROR_STOP=off"}
		if len(dbs) == 1 {
			args = append(args, "-d", dbs[0])
		} else {
			args = append(args, "-d", "postgres")
		}
		return args, nil

	case MySQL:
		args := []string{"mysql"}
		if s.User != "" {
			args = append(args, "-u", s.User)
		}
		if s.Password != "" {
			args = append(args, "-p"+s.Password)
		}
		if len(dbs) == 1 {
			args = append(args, dbs[0])
		}
		return args, nil

	case MongoDB:
		args := []string{"mongorestore", "--archive", "--drop"}
		if s.User != "" {
			args = append(args, "-u", s.User, "--authenticationDatabase", "admin")
		}
		if s.Password != "" {
			args = append(args, "-p", s.Password)
		}
		return args, nil

	default:
		return nil, fmt.Errorf("backups: unknown engine %q", s.Engine)
	}
}

// Extension names the object so that a human staring at a bucket listing can tell what
// they are looking at, and so the restore path knows what to undo.
func Extension(e Engine, encrypted bool) string {
	base := ".sql.gz"
	switch e {
	case MongoDB:
		base = ".archive.gz"
	case Volume:
		base = ".tar.gz"
	}
	if encrypted {
		base += ".age"
	}
	return base
}

func splitDatabases(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
