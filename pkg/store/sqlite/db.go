// Package sqlite provides SQLite-backed implementations of the store interfaces.
// The driver used is modernc.org/sqlite (pure Go, no CGo required).
package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // register "sqlite" driver
)

// Open opens (or creates) the SQLite database at dsn, applies WAL pragmas,
// runs schema migrations, and resets any stale IN_PROGRESS jobs to PENDING.
//
// dsn may be:
//   - A file path:  "./data/sandrpod.db"
//   - In-memory:    ":memory:"  (useful for tests)
func Open(dsn string) (*sql.DB, error) {
	if dsn != ":memory:" {
		dir := filepath.Dir(dsn)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store/sqlite: create data dir %q: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: open %q: %w", dsn, err)
	}

	// SQLite supports only one writer at a time. With WAL mode a single
	// connection is the simplest and safest configuration: no writer contention,
	// no SQLITE_BUSY, and concurrent reads from the same connection are safe.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		`PRAGMA journal_mode=WAL`,   // concurrent reads while writing
		`PRAGMA busy_timeout=5000`,  // wait up to 5 s instead of returning SQLITE_BUSY
		`PRAGMA foreign_keys=ON`,    // enforce FK constraints
		`PRAGMA synchronous=NORMAL`, // safe with WAL; faster than FULL
		`PRAGMA cache_size=-8000`,   // 8 MB page cache
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("store/sqlite: set pragma %q: %w", p, err)
		}
	}

	if err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store/sqlite: migrate: %w", err)
	}

	// Startup recovery: jobs that were IN_PROGRESS when the server died are
	// returned to PENDING so a Poder agent can re-claim them.
	if _, err := db.Exec(
		`UPDATE jobs SET status='PENDING', updated_at=datetime('now') WHERE status='IN_PROGRESS'`,
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("store/sqlite: startup recovery: %w", err)
	}

	return db, nil
}
