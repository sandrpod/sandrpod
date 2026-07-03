package sqldb

import (
	"database/sql"
	"strconv"
	"strings"
)

// Dialect captures the handful of ways SQLite and PostgreSQL diverge behind the
// otherwise-shared repositories: placeholder style, schema DDL, legacy column
// migrations, duplicate-column detection, and the row-locking clause used to
// claim jobs concurrently. Everything else — the queries themselves, the
// portable ON CONFLICT upserts, and RFC3339-string timestamps — is identical.
type Dialect interface {
	// Rebind converts '?' placeholders to the dialect's parameter style.
	Rebind(query string) string
	// Schema returns the CREATE TABLE / INDEX (IF NOT EXISTS) DDL.
	Schema() string
	// Migrations returns idempotent ADD COLUMN statements for pre-existing DBs.
	Migrations() []string
	// IsDupColumn reports whether err is "add a column that already exists".
	IsDupColumn(err error) bool
	// ClaimLock is appended to the PENDING-job SELECT so concurrent pollers
	// don't double-claim a job. Empty for SQLite (a single writer serialises).
	ClaimLock() string
}

// ─── SQLite ──────────────────────────────────────────────────────────────────

type sqliteDialect struct{}

func (sqliteDialect) Rebind(q string) string { return q } // '?' is native
func (sqliteDialect) Schema() string         { return ddl }
func (sqliteDialect) Migrations() []string   { return columnMigrations }
func (sqliteDialect) IsDupColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}
func (sqliteDialect) ClaimLock() string { return "" }

// ─── PostgreSQL ──────────────────────────────────────────────────────────────

type pgDialect struct{}

func (pgDialect) Rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// pgTypes adapts SQLite's DDL types to Postgres: DATETIME (TEXT affinity storing
// RFC3339 strings) has no PG type → TEXT (the UTC strings round-trip and still
// sort chronologically); INTEGER is 32-bit in PG but 64-bit in SQLite → BIGINT,
// so 64-bit values like memory_bytes don't overflow int4.
func pgTypes(ddl string) string {
	ddl = strings.ReplaceAll(ddl, "DATETIME", "TEXT")
	ddl = strings.ReplaceAll(ddl, "INTEGER", "BIGINT")
	return ddl
}

func (pgDialect) Schema() string { return pgTypes(ddl) }

func (pgDialect) Migrations() []string {
	out := make([]string, len(columnMigrations))
	for i, m := range columnMigrations {
		m = strings.Replace(m, "ADD COLUMN ", "ADD COLUMN IF NOT EXISTS ", 1)
		out[i] = pgTypes(m)
	}
	return out
}

func (pgDialect) IsDupColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

// ClaimLock uses SKIP LOCKED so N concurrent pollers each claim a disjoint set
// of PENDING jobs — the concurrency that SQLite's single writer can't provide.
func (pgDialect) ClaimLock() string { return " FOR UPDATE SKIP LOCKED" }

// ─── DB / Tx wrappers: auto-rebind placeholders per dialect ───────────────────

// DB wraps *sql.DB so the repositories keep writing '?' placeholders regardless
// of dialect; Rebind is applied on every call. Query results are the standard
// *sql.Row / *sql.Rows, so the scan helpers need no changes.
type DB struct {
	sdb *sql.DB
	d   Dialect
}

func (db *DB) Exec(q string, args ...any) (sql.Result, error) {
	return db.sdb.Exec(db.d.Rebind(q), args...)
}
func (db *DB) Query(q string, args ...any) (*sql.Rows, error) {
	return db.sdb.Query(db.d.Rebind(q), args...)
}
func (db *DB) QueryRow(q string, args ...any) *sql.Row {
	return db.sdb.QueryRow(db.d.Rebind(q), args...)
}
func (db *DB) Begin() (*Tx, error) {
	tx, err := db.sdb.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, d: db.d}, nil
}
func (db *DB) Close() error { return db.sdb.Close() }

// Tx wraps *sql.Tx with the same auto-rebinding.
type Tx struct {
	tx *sql.Tx
	d  Dialect
}

func (tx *Tx) Exec(q string, args ...any) (sql.Result, error) {
	return tx.tx.Exec(tx.d.Rebind(q), args...)
}
func (tx *Tx) Query(q string, args ...any) (*sql.Rows, error) {
	return tx.tx.Query(tx.d.Rebind(q), args...)
}
func (tx *Tx) QueryRow(q string, args ...any) *sql.Row {
	return tx.tx.QueryRow(tx.d.Rebind(q), args...)
}
func (tx *Tx) Commit() error   { return tx.tx.Commit() }
func (tx *Tx) Rollback() error { return tx.tx.Rollback() }
