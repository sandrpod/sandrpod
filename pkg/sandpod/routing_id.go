// Copyright 2026 SandrPod Contributors
//
// Routing keys for sandboxes.
//
// The name a person chooses is theirs, and two workspaces may both want
// "mysandbox". Everything that has to be unique across the whole server — the
// wildcard Host (<port>-<key>.<domain>), the tunnel key, the container name, the
// E2B sandbox id — therefore uses an opaque key instead. The user-facing name is
// resolved within the caller's workspace.
package sandpod

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewRoutingID returns an immutable, globally unique routing key. The prefix keeps
// it distinguishable from a legacy row whose key is still the old user-facing
// name, and the length fits comfortably in a DNS label with the port prefix.
func NewRoutingID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A broken RNG is a broken machine; a timestamp still gives uniqueness
		// within one server, which is all a routing key needs.
		return fmt.Sprintf("sb-%016x", time.Now().UnixNano())
	}
	return "sb-" + hex.EncodeToString(b[:])
}

// DisplayNameOf returns the user-facing name, falling back to the routing key
// for records created before names were separated from keys.
func (sb *SandboxInfo) DisplayNameOf() string {
	if sb.DisplayName != "" {
		return sb.DisplayName
	}
	return sb.Name
}
