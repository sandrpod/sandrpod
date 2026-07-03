package sqldb

import (
	"fmt"
	"time"
)

type tunnelOwnerRepo struct{ db *DB }

// NewTunnelOwnerRepo returns a SQL-backed TunnelOwnerRepository.
func NewTunnelOwnerRepo(db *DB) *tunnelOwnerRepo { return &tunnelOwnerRepo{db: db} }

func (r *tunnelOwnerRepo) Claim(key, nodeURL string) error {
	_, err := r.db.Exec(
		`INSERT INTO tunnel_owners (tunnel_key, node_url, updated_at) VALUES (?,?,?)
		 ON CONFLICT(tunnel_key) DO UPDATE SET node_url=excluded.node_url, updated_at=excluded.updated_at`,
		key, nodeURL, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("store/sqldb: tunnel Claim %q: %w", key, err)
	}
	return nil
}

func (r *tunnelOwnerRepo) Release(key, nodeURL string) error {
	if _, err := r.db.Exec(
		`DELETE FROM tunnel_owners WHERE tunnel_key=? AND node_url=?`, key, nodeURL,
	); err != nil {
		return fmt.Errorf("store/sqldb: tunnel Release %q: %w", key, err)
	}
	return nil
}

func (r *tunnelOwnerRepo) NodeFor(key string) (string, bool) {
	var nodeURL string
	if err := r.db.QueryRow(
		`SELECT node_url FROM tunnel_owners WHERE tunnel_key=?`, key,
	).Scan(&nodeURL); err != nil {
		return "", false
	}
	return nodeURL, true
}
