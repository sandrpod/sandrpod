package sqldb

import "database/sql"

// ddl is the schema definition. All statements use IF NOT EXISTS so Migrate
// is idempotent and safe to call on every server startup.
//
// Design notes:
//   - DATETIME columns store RFC3339 strings; this is standard SQLite practice
//     and the syntax is accepted unchanged by PostgreSQL TIMESTAMP WITH TIME ZONE.
//   - sandboxes.labels and jobs.trace_context are JSON TEXT (map[string]string).
//   - jobs.result is nullable JSON TEXT (*JobResult); NULL means no result yet.
//   - poders stores Resources and Usage as inline columns (not JSON) so that
//     the SelectBest scoring query can run entirely in SQL without a table scan.
const ddl = `
CREATE TABLE IF NOT EXISTS sandboxes (
    name           TEXT PRIMARY KEY,
    id             TEXT    NOT NULL DEFAULT '',
    region         TEXT    NOT NULL DEFAULT '',
    provider_type  TEXT    NOT NULL DEFAULT '',
    instance_type  TEXT    NOT NULL DEFAULT '',
    image_id       TEXT    NOT NULL DEFAULT '',
    state          TEXT    NOT NULL DEFAULT 'PENDING',
    ip             TEXT    NOT NULL DEFAULT '',
    poder_id       TEXT    NOT NULL DEFAULT '',
    poder_url      TEXT    NOT NULL DEFAULT '',
    container_id   TEXT    NOT NULL DEFAULT '',
    proxy_url      TEXT    NOT NULL DEFAULT '',
    api_url        TEXT    NOT NULL DEFAULT '',
    arch           TEXT    NOT NULL DEFAULT '',
    os             TEXT    NOT NULL DEFAULT '',
    os_version     TEXT    NOT NULL DEFAULT '',
    labels         TEXT    NOT NULL DEFAULT '{}',
    owner          TEXT    NOT NULL DEFAULT '',
    ttl_seconds    INTEGER NOT NULL DEFAULT 0,
    created_at     DATETIME NOT NULL,
    last_activity  DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS poders (
    id               TEXT PRIMARY KEY,
    name             TEXT    NOT NULL DEFAULT '',
    url              TEXT    NOT NULL DEFAULT '',
    region           TEXT    NOT NULL DEFAULT '',
    provider_type    TEXT    NOT NULL DEFAULT '',
    vm_id            TEXT    NOT NULL DEFAULT '',
    state            TEXT    NOT NULL DEFAULT 'OFFLINE',
    -- PoderResources (inline for SQL scoring)
    cpu_cores        INTEGER NOT NULL DEFAULT 0,
    memory_bytes     INTEGER NOT NULL DEFAULT 0,
    max_containers   INTEGER NOT NULL DEFAULT 0,
    arch             TEXT    NOT NULL DEFAULT '',
    os               TEXT    NOT NULL DEFAULT '',
    os_version       TEXT    NOT NULL DEFAULT '',
    kernel_version   TEXT    NOT NULL DEFAULT '',
    -- PoderUsage (inline for SQL scoring)
    usage_containers INTEGER NOT NULL DEFAULT 0,
    usage_cpu        REAL    NOT NULL DEFAULT 0,
    usage_memory     REAL    NOT NULL DEFAULT 0,
    last_heartbeat   DATETIME NOT NULL,
    created_at       DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
    id             TEXT PRIMARY KEY,
    type           TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL DEFAULT 'PENDING',
    sandbox_name   TEXT    NOT NULL DEFAULT '',
    sandbox_id     TEXT    NOT NULL DEFAULT '',
    region         TEXT    NOT NULL DEFAULT '',
    provider_type  TEXT    NOT NULL DEFAULT '',
    poder_id       TEXT    NOT NULL DEFAULT '',
    poder_url      TEXT    NOT NULL DEFAULT '',
    vm_id          TEXT    NOT NULL DEFAULT '',
    instance_type  TEXT    NOT NULL DEFAULT '',
    image_id       TEXT    NOT NULL DEFAULT '',
    command        TEXT    NOT NULL DEFAULT '',
    language       TEXT    NOT NULL DEFAULT '',
    result         TEXT,
    error_message  TEXT    NOT NULL DEFAULT '',
    trace_context  TEXT    NOT NULL DEFAULT '{}',
    owner          TEXT    NOT NULL DEFAULT '',
    created_at     DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS api_tokens (
    hash        TEXT PRIMARY KEY,           -- sha256(raw key), hex; raw key never stored
    name        TEXT     NOT NULL DEFAULT '',
    prefix      TEXT     NOT NULL DEFAULT '', -- first chars of the key, for display/revoke
    role        TEXT     NOT NULL DEFAULT 'user',
    created_at  DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_status      ON jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_updated     ON jobs(status, updated_at);
CREATE INDEX IF NOT EXISTS idx_sandboxes_poder  ON sandboxes(poder_id);
CREATE INDEX IF NOT EXISTS idx_poders_state     ON poders(state, region, provider_type);
CREATE INDEX IF NOT EXISTS idx_api_tokens_prefix ON api_tokens(prefix);
`

// columnMigrations lists ADD COLUMN statements applied to existing databases
// whose tables predate a column added to the CREATE TABLE DDL above. They run
// after the idempotent DDL; "duplicate column name" errors are ignored so the
// migration is safe to re-run on already-migrated databases.
var columnMigrations = []string{
	`ALTER TABLE poders ADD COLUMN vm_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sandboxes ADD COLUMN owner TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN owner TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sandboxes ADD COLUMN ttl_seconds INTEGER NOT NULL DEFAULT 0`,
}

// migrate applies the dialect's schema then its idempotent column migrations.
// It is safe to call on every startup (CREATE … IF NOT EXISTS; duplicate-column
// ADDs are ignored per the dialect).
func migrate(sdb *sql.DB, d Dialect) error {
	if _, err := sdb.Exec(d.Schema()); err != nil {
		return err
	}
	for _, stmt := range d.Migrations() {
		if _, err := sdb.Exec(stmt); err != nil && !d.IsDupColumn(err) {
			return err
		}
	}
	return nil
}
