package sandpod

import "time"

// APIToken is a persisted API credential. The raw secret is never stored — only
// its SHA-256 hash — so a leaked database yields no usable keys. Prefix keeps
// the key's first characters for display and revocation without exposing it.
//
// A token authenticates the same on the native API and the E2B gateway (both go
// through the server's token resolver), and issued keys use E2B's canonical
// e2b_<hex> shape so they work as a drop-in E2B_API_KEY with no extra client
// config.
type APIToken struct {
	Name      string    `json:"name"`       // owner identity / label
	Prefix    string    `json:"prefix"`     // e.g. "e2b_1a2b3c4d5e6f" (display only)
	Hash      string    `json:"-"`          // sha256(raw key) hex; never serialized
	Role      string    `json:"role"`       // "admin" | "user"
	CreatedAt time.Time `json:"created_at"`
}

// APITokenRepository is the read/write contract for persisted API tokens.
// Implementations must be safe for concurrent use.
type APITokenRepository interface {
	// Create persists a token. It must reject a duplicate Hash.
	Create(t *APIToken) error
	List() ([]*APIToken, error)
	// DeleteByPrefix removes every token whose Prefix matches and returns their
	// hashes, so an in-memory auth index can drop the same entries. removed is
	// empty when nothing matched.
	DeleteByPrefix(prefix string) (removed []string, err error)
}
