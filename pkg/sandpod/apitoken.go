package sandpod

import (
	"errors"
	"time"
)

// APIToken is a persisted API credential. The raw secret is never stored — only
// its SHA-256 hash — so a leaked database yields no usable keys. Prefix keeps
// the key's first characters for display and revocation without exposing it.
//
// A token authenticates the same on the native API and the E2B gateway (both go
// through the server's token resolver), and issued keys use E2B's canonical
// e2b_<hex> shape so they work as a drop-in E2B_API_KEY with no extra client
// config.
type APIToken struct {
	Name      string    `json:"name"`   // owner identity / label
	Prefix    string    `json:"prefix"` // e.g. "e2b_1a2b3c4d5e6f" (display only)
	Hash      string    `json:"-"`      // sha256(raw key) hex; never serialized
	Role      string    `json:"role"`   // "admin" | "user"
	CreatedAt time.Time `json:"created_at"`
}

// APITokenRepository is the read/write contract for persisted API tokens.
// Implementations must be safe for concurrent use.
type APITokenRepository interface {
	// Create persists a token. It must reject a duplicate Hash.
	Create(t *APIToken) error
	List() ([]*APIToken, error)
	// FindByHash returns the token with the given SHA-256 hash, if any. It lets
	// an instance validate a token issued on a peer (its local cache misses).
	FindByHash(hash string) (*APIToken, bool)
	// DeleteByPrefix removes every token whose Prefix matches and returns their
	// hashes, so an in-memory auth index can drop the same entries. removed is
	// empty when nothing matched.
	DeleteByPrefix(prefix string) (removed []string, err error)
}

// ErrLastToken is returned by LastTokenGuard.DeleteByPrefixIfNotLast when the
// delete would leave the table empty.
var ErrLastToken = errors.New("refusing to delete the last API token")

// LastTokenGuard is an optional capability on an APITokenRepository: delete a
// prefix only if at least one other token remains, decided atomically with the
// delete. The plain check-then-delete in the HTTP handler is a race two
// concurrent revocations can win, leaving a server with no credential at all.
type LastTokenGuard interface {
	// DeleteByPrefixIfNotLast behaves like DeleteByPrefix but returns
	// ErrLastToken (and removes nothing) when every remaining row belongs to
	// prefix. A prefix that matches nothing returns an empty removed slice.
	DeleteByPrefixIfNotLast(prefix string) (removed []string, err error)
}
