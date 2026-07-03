package sqldb

import (
	"fmt"
	"time"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
)

type tokenRepo struct{ db *DB }

// NewTokenRepo returns a SQLite-backed APITokenRepository.
func NewTokenRepo(db *DB) *tokenRepo { return &tokenRepo{db: db} }

func (r *tokenRepo) Create(t *sandpod.APIToken) error {
	_, err := r.db.Exec(
		`INSERT INTO api_tokens (hash, name, prefix, role, created_at) VALUES (?,?,?,?,?)`,
		t.Hash, t.Name, t.Prefix, t.Role, t.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("store/sqlite: api token Create %q: %w", t.Prefix, err)
	}
	return nil
}

func (r *tokenRepo) List() ([]*sandpod.APIToken, error) {
	rows, err := r.db.Query(
		`SELECT hash, name, prefix, role, created_at FROM api_tokens ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: api token List: %w", err)
	}
	defer rows.Close()
	var out []*sandpod.APIToken
	for rows.Next() {
		var t sandpod.APIToken
		var created string
		if err := rows.Scan(&t.Hash, &t.Name, &t.Prefix, &t.Role, &created); err != nil {
			return nil, fmt.Errorf("store/sqlite: api token scan: %w", err)
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (r *tokenRepo) DeleteByPrefix(prefix string) ([]string, error) {
	// Read the matching hashes first so the caller can drop them from its
	// in-memory auth index, then delete in the same transaction.
	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: api token delete begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT hash FROM api_tokens WHERE prefix=?`, prefix)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: api token select %q: %w", prefix, err)
	}
	var removed []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, err
		}
		removed = append(removed, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(`DELETE FROM api_tokens WHERE prefix=?`, prefix); err != nil {
		return nil, fmt.Errorf("store/sqlite: api token delete %q: %w", prefix, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store/sqlite: api token delete commit: %w", err)
	}
	return removed, nil
}
