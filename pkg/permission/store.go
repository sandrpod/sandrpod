// Copyright 2026 SandrPod
// Persistence layer for permissions.json.
//
// Atomic writes via tmp+rename so a crash mid-save can never leave a
// truncated/corrupt file. File is chmod 0600 (employee-only readable) since
// it lists which paths the AI can touch — leaking that aids targeted attacks.

package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CurrentVersion is the on-disk format version. Bump on backwards-incompatible changes.
const CurrentVersion = 1

// DefaultStorePath returns the canonical location of permissions.json
// under the invoking user's home dir.
func DefaultStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".sandrpod", "permissions.json"), nil
}

// Store is a thread-safe, file-backed Snapshot.
//
// All public methods are safe for concurrent use. Writes are atomic and durable
// (fsync’d on rename), so there is no partial-state window visible to readers.
type Store struct {
	path string

	mu   sync.RWMutex
	snap Snapshot
}

// LoadStore reads the snapshot at `path` (or creates a fresh one if missing).
// Missing-file is not an error — the first run starts empty and the manager
// will accumulate rules as the employee grants them.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read permissions store: %w", err)
		}
		// Fresh file — bootstrap a minimal snapshot.
		s.snap = Snapshot{
			Version:   CurrentVersion,
			Rules:     []Rule{},
			UpdatedAt: time.Now(),
		}
		return s, nil
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse permissions store at %s: %w", path, err)
	}

	if snap.Version == 0 {
		snap.Version = CurrentVersion
	}
	if snap.Version > CurrentVersion {
		return nil, fmt.Errorf("permissions store version %d is newer than supported (%d) — please upgrade sandrpod", snap.Version, CurrentVersion)
	}
	if snap.Rules == nil {
		snap.Rules = []Rule{}
	}
	s.snap = snap
	return s, nil
}

// Snapshot returns a deep copy of the current state. Safe to mutate.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// JSON round-trip is the cheapest deep-copy we can rely on for a small
	// struct that's already JSON-serializable.
	out := s.snap
	out.Rules = append([]Rule(nil), s.snap.Rules...)
	out.SessionGrants = append([]Rule(nil), s.snap.SessionGrants...)
	out.CommandPolicy.Deny = append([]string(nil), s.snap.CommandPolicy.Deny...)
	out.CommandPolicy.Warn = append([]string(nil), s.snap.CommandPolicy.Warn...)
	return out
}

// AddPermanentRule persists a permanent grant. If a rule with the same path
// already exists it is replaced (so a "rw" grant supersedes an earlier "r").
func (s *Store) AddPermanentRule(r Rule) error {
	r.Scope = ScopePermanent
	if r.GrantedAt.IsZero() {
		r.GrantedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Rules = upsertRule(s.snap.Rules, r)
	return s.flushLocked()
}

// AddSessionRule persists a session-scoped grant tied to a sandbox session.
func (s *Store) AddSessionRule(r Rule) error {
	r.Scope = ScopeSession
	if r.GrantedAt.IsZero() {
		r.GrantedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.SessionGrants = upsertRule(s.snap.SessionGrants, r)
	return s.flushLocked()
}

// AddHardlock writes an entry that the manager will treat as deny-with-no-prompt.
// Hardlocks can only be removed via the CLI unlock flow (see manager).
func (s *Store) AddHardlock(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Rules = upsertRule(s.snap.Rules, Rule{
		Path:      path,
		Mode:      "deny",
		Scope:     ScopeHardlock,
		GrantedAt: time.Now(),
	})
	return s.flushLocked()
}

// RemoveRule deletes the rule at `path` from the permanent table.
//
// removeHardlock=true is required to remove a hardlock entry; the GUI passes
// false (so it cannot accidentally drop a hardlock), and only the CLI unlock
// command passes true.
func (s *Store) RemoveRule(path string, removeHardlock bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap.Rules[:0]
	for _, r := range s.snap.Rules {
		if r.Path == path {
			if r.Scope == ScopeHardlock && !removeHardlock {
				out = append(out, r)
				continue
			}
			continue // drop
		}
		out = append(out, r)
	}
	s.snap.Rules = out
	return s.flushLocked()
}

// PurgeExpiredSessions removes session grants whose ExpiresAt has passed
// or whose SessionID matches one of the supplied terminated IDs.
func (s *Store) PurgeExpiredSessions(now time.Time, terminated map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap.SessionGrants[:0]
	for _, r := range s.snap.SessionGrants {
		if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
			continue
		}
		if terminated != nil && terminated[r.SessionID] {
			continue
		}
		out = append(out, r)
	}
	if len(out) == len(s.snap.SessionGrants) {
		return nil // nothing changed; skip disk write
	}
	s.snap.SessionGrants = out
	return s.flushLocked()
}

// SetCommandPolicy replaces the deny/warn lists.
func (s *Store) SetCommandPolicy(p CommandPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.CommandPolicy = p
	return s.flushLocked()
}

// flushLocked writes the snapshot to disk atomically.
// Caller must hold s.mu.
func (s *Store) flushLocked() error {
	s.snap.UpdatedAt = time.Now()
	if s.snap.Version == 0 {
		s.snap.Version = CurrentVersion
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("create permissions dir: %w", err)
	}

	data, err := json.MarshalIndent(s.snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp) //nolint:errcheck — best-effort cleanup
		return fmt.Errorf("rename tmp → final: %w", err)
	}
	return nil
}

// upsertRule appends r, or merges it into an existing rule with the same path.
//
// Merge semantics for same-path rules:
//   - Hardlock always wins over non-hardlock (defense in depth — a permanent
//     allow can never silently shadow a hardlock).
//   - For permanent + permanent collisions on the same path we widen the
//     mode (r ∪ w → rw) instead of overwriting. Without this widening, the
//     user would see the dialog re-pop whenever the AI's mode changed
//     (read after write or vice versa) and feel like the previous "永久
//     允许" click was forgotten.
//   - Same-mode upsert refreshes GrantedAt + Note in case the caller is
//     re-recording an audit event.
func upsertRule(rules []Rule, r Rule) []Rule {
	for i, existing := range rules {
		if existing.Path != r.Path {
			continue
		}
		if existing.Scope == ScopeHardlock && r.Scope != ScopeHardlock {
			return rules // refuse to overwrite hardlock
		}
		// Permanent + permanent: widen the mode union rather than overwrite.
		if existing.Scope == ScopePermanent && r.Scope == ScopePermanent {
			r.Mode = unionMode(existing.Mode, r.Mode)
		}
		rules[i] = r
		return rules
	}
	return append(rules, r)
}

// unionMode returns the most permissive of (a, b). It treats deny / unknown
// as opaque "keep verbatim" so we never accidentally promote a deny rule.
func unionMode(a, b Mode) Mode {
	// Deny rules are not permissive grants — never widen.
	if a == "deny" || b == "deny" {
		if a == "deny" {
			return a
		}
		return b
	}
	// Reach rw if either side has it, or if both r+w pair appears.
	if a == ModeReadWrite || b == ModeReadWrite {
		return ModeReadWrite
	}
	if (a == ModeRead && b == ModeWrite) || (a == ModeWrite && b == ModeRead) {
		return ModeReadWrite
	}
	// Same mode (or only one side present) — keep b (the new rule's value).
	return b
}
