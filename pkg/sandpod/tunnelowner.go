package sandpod

// TunnelOwnerRepository tracks which server instance currently holds the live
// reverse tunnel for a given key (a poder id, or a direct-agent sandbox name).
// In a load-balanced, multi-instance deployment a request can land on any
// instance, but the yamux tunnel lives on exactly one; the owner map lets the
// receiving instance forward the request to the node that can reach the poder.
//
// It is only meaningful with a shared backend (PostgreSQL). Single-instance and
// in-memory/SQLite deployments never consult it (the tunnel is always local).
type TunnelOwnerRepository interface {
	// Claim records that nodeURL now holds key's tunnel (upsert).
	Claim(key, nodeURL string) error
	// Release removes key's ownership, but only if still held by nodeURL, so a
	// stale disconnect can't clobber a fresh reconnect on another node.
	Release(key, nodeURL string) error
	// NodeFor returns the node URL currently holding key's tunnel, if any.
	NodeFor(key string) (string, bool)
}
