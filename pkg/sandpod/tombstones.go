// Copyright 2026 SandrPod Contributors
// Tombstones - a small expiring set of recently-deleted poder IDs.
//
// When a poder is deleted and its VM terminated, the poder container often
// survives a few more seconds and reconnects, re-registering itself and
// leaving a ghost OFFLINE record once the VM finally dies. Tombstoning the ID
// for a grace window lets the server reject that re-registration.

package sandpod

import (
	"sync"
	"time"
)

// Tombstones is a concurrency-safe set of IDs that expire after a TTL.
type Tombstones struct {
	mu  sync.Mutex
	m   map[string]time.Time // id -> expiry
	ttl time.Duration
}

// NewTombstones creates a tombstone set whose entries expire after ttl.
func NewTombstones(ttl time.Duration) *Tombstones {
	return &Tombstones{m: make(map[string]time.Time), ttl: ttl}
}

// Add tombstones an ID for the configured TTL.
func (t *Tombstones) Add(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[id] = time.Now().Add(t.ttl)
	// Opportunistic prune so the map can't grow unbounded.
	for k, exp := range t.m {
		if time.Now().After(exp) {
			delete(t.m, k)
		}
	}
}

// Contains reports whether an ID is currently tombstoned.
func (t *Tombstones) Contains(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.m[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(t.m, id)
		return false
	}
	return true
}
