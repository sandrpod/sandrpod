// Package sqldb provides SQLite- and PostgreSQL-backed implementations of the
// pkg/sandpod store interfaces behind one dialect-parameterised codebase. The
// repositories write '?'-placeholder SQL and portable ON CONFLICT upserts; a
// thin Dialect (see dialect.go) handles the few divergences — placeholder
// rebinding, DDL types, and the SKIP LOCKED job-claim clause.
package sqldb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver
	_ "modernc.org/sqlite"             // register "sqlite" driver (pure Go, no CGo)
)

// Open opens a persistence backend selected by the DSN scheme:
//
//	postgres://…  |  postgresql://…   → PostgreSQL (connection pool)
//	sqlite:<path>                     → SQLite file
//	<path>  |  :memory:               → SQLite (bare path; back-compat + tests)
//
// It applies driver-appropriate settings, runs migrations, and resets any stale
// IN_PROGRESS jobs to PENDING.
func Open(dsn string) (*DB, error) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return openPostgres(dsn)
	default:
		return openSQLite(strings.TrimPrefix(dsn, "sqlite:"))
	}
}

func openSQLite(path string) (*DB, error) {
	if path != ":memory:" && path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("store/sqldb: create data dir: %w", err)
		}
	}
	sdb, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store/sqldb: open sqlite %q: %w", path, err)
	}
	// SQLite has a single writer; one connection in WAL mode avoids SQLITE_BUSY
	// and makes the PollJobs claim safe without row locks.
	sdb.SetMaxOpenConns(1)
	for _, p := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA cache_size=-8000`,
	} {
		if _, err := sdb.Exec(p); err != nil {
			sdb.Close()
			return nil, fmt.Errorf("store/sqldb: pragma %q: %w", p, err)
		}
	}
	return finish(sdb, sqliteDialect{})
}

func openPostgres(dsn string) (*DB, error) {
	sdb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store/sqldb: open postgres: %w", err)
	}
	// A real connection pool — the reason to move off SQLite. Concurrent writers
	// are safe because PollJobs claims rows with FOR UPDATE SKIP LOCKED.
	sdb.SetMaxOpenConns(20)
	sdb.SetMaxIdleConns(5)
	sdb.SetConnMaxLifetime(30 * time.Minute)
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("store/sqldb: ping postgres: %w", err)
	}
	return finish(sdb, pgDialect{})
}

// finish runs migrations + startup recovery and wraps the pool with its dialect.
func finish(sdb *sql.DB, d Dialect) (*DB, error) {
	// Retry: when several instances boot against a fresh Postgres simultaneously,
	// concurrent CREATE TABLE IF NOT EXISTS can conflict; the loser retries and
	// finds the tables already there (idempotent DDL).
	var mErr error
	for attempt := 0; attempt < 3; attempt++ {
		if mErr = migrate(sdb, d); mErr == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if mErr != nil {
		sdb.Close()
		return nil, fmt.Errorf("store/sqldb: migrate: %w", mErr)
	}
	db := &DB{sdb: sdb, d: d}
	// Jobs left IN_PROGRESS by a crashed server return to PENDING so a poder can
	// re-claim them. A Go-formatted timestamp keeps the value dialect-portable.
	if _, err := db.Exec(
		`UPDATE jobs SET status='PENDING', updated_at=? WHERE status='IN_PROGRESS'`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("store/sqldb: startup recovery: %w", err)
	}
	return db, nil
}
