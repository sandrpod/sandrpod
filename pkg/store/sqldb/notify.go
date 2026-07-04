package sqldb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// tokenChannel is the Postgres LISTEN/NOTIFY channel over which instances
// announce API-token issuance/revocation, so a peer's in-memory auth index
// converges immediately instead of waiting for the periodic reload. This closes
// the ≤30s eventual-revocation window in multi-instance deployments.
const tokenChannel = "sandrpod_tokens"

// isPostgres reports whether this DB is Postgres-backed (LISTEN/NOTIFY is a
// no-op on SQLite, which is single-instance by construction).
func (db *DB) isPostgres() bool {
	_, ok := db.d.(pgDialect)
	return ok
}

// NotifyTokensChanged announces that the API-token set changed so peer instances
// reload their in-memory index. Called after an issue or revoke commits. No-op
// on SQLite (single instance — nothing to notify).
func (db *DB) NotifyTokensChanged() error {
	if !db.isPostgres() {
		return nil
	}
	// pg_notify avoids the string-literal-only payload restriction of NOTIFY and
	// runs over the pooled connection. The payload is unused; any signal triggers
	// a full reload on the listener side, which is uniform for issue and revoke.
	_, err := db.sdb.Exec("SELECT pg_notify('" + tokenChannel + "', '')")
	return err
}

// ListenTokensChanged blocks, invoking onChange whenever any instance announces
// a token change on tokenChannel, until ctx is cancelled. It holds a dedicated
// connection (LISTEN cannot share the pool) and reconnects with backoff on
// connection loss — the caller's periodic reload is the backstop during a gap.
// No-op on SQLite.
func (db *DB) ListenTokensChanged(ctx context.Context, onChange func()) error {
	if !db.isPostgres() || db.dsn == "" {
		return nil
	}
	for ctx.Err() == nil {
		if err := db.listenTokensOnce(ctx, onChange); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second): // dropped connection: back off, then reconnect
			}
		}
	}
	return ctx.Err()
}

func (db *DB) listenTokensOnce(ctx context.Context, onChange func()) error {
	conn, err := pgx.Connect(ctx, db.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+tokenChannel); err != nil {
		return err
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err // ctx cancelled or connection lost — caller reconnects
		}
		onChange()
	}
}
