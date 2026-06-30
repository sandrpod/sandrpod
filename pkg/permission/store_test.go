// Copyright 2026 SandrPod
// Tests for the file-backed Store: persistence, atomic writes, reload
// consistency, rule upsert/merge semantics, and concurrency safety.

package permission

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "permissions.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store, path
}

func TestLoadStore_FreshFile_BootstrapsEmptySnapshot(t *testing.T) {
	store, path := newTempStore(t)

	// No file should exist yet (LoadStore on a missing path does not write).
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("LoadStore must not create the file before first write; stat err=%v", err)
	}

	snap := store.Snapshot()
	if snap.Version != CurrentVersion {
		t.Errorf("fresh snapshot version = %d, want %d", snap.Version, CurrentVersion)
	}
	// Note: Snapshot() deep-copies via append([]Rule(nil), ...), so an empty
	// rule set comes back as a nil slice — len()==0 is the contract, not non-nil.
	if len(snap.Rules) != 0 {
		t.Errorf("fresh snapshot should have 0 rules, got %d", len(snap.Rules))
	}
}

func TestStore_WriteThenReload_RoundTrips(t *testing.T) {
	store, path := newTempStore(t)

	if err := store.AddPermanentRule(Rule{Path: "/Users/test/Documents", Mode: ModeReadWrite}); err != nil {
		t.Fatalf("AddPermanentRule: %v", err)
	}
	if err := store.AddHardlock("/Users/test/.ssh"); err != nil {
		t.Fatalf("AddHardlock: %v", err)
	}
	if err := store.SetCommandPolicy(CommandPolicy{Deny: []string{"scp"}, Warn: []string{"curl"}}); err != nil {
		t.Fatalf("SetCommandPolicy: %v", err)
	}

	// File must exist on disk after a write and be chmod 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after write: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("permissions.json mode = %o, want 0600", perm)
	}

	// Reload from disk into a fresh Store and assert state survived.
	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload LoadStore: %v", err)
	}
	snap := reloaded.Snapshot()
	if len(snap.Rules) != 2 {
		t.Fatalf("reloaded rules = %d, want 2 (%+v)", len(snap.Rules), snap.Rules)
	}
	if len(snap.CommandPolicy.Deny) != 1 || snap.CommandPolicy.Deny[0] != "scp" {
		t.Errorf("command policy deny not round-tripped: %+v", snap.CommandPolicy)
	}
	if len(snap.CommandPolicy.Warn) != 1 || snap.CommandPolicy.Warn[0] != "curl" {
		t.Errorf("command policy warn not round-tripped: %+v", snap.CommandPolicy)
	}
}

func TestLoadStore_CorruptJSON_ReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if _, err := LoadStore(path); err == nil {
		t.Error("LoadStore must error on corrupt JSON")
	}
}

func TestLoadStore_FutureVersion_Rejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.json")
	if err := os.WriteFile(path, []byte(`{"version":999,"rules":[]}`), 0600); err != nil {
		t.Fatalf("seed future-version file: %v", err)
	}
	if _, err := LoadStore(path); err == nil {
		t.Error("LoadStore must reject a store version newer than supported")
	}
}

func TestLoadStore_ZeroVersionNormalized_AndNilRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.json")
	// version omitted (0) and rules omitted (nil) — both should be normalized.
	if err := os.WriteFile(path, []byte(`{"updated_at":"2020-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	snap := store.Snapshot()
	if snap.Version != CurrentVersion {
		t.Errorf("version 0 should normalize to %d, got %d", CurrentVersion, snap.Version)
	}
	if len(snap.Rules) != 0 {
		t.Errorf("nil rules should normalize to 0-length, got %d", len(snap.Rules))
	}
}

func TestAddPermanentRule_WidensModeOnSamePath(t *testing.T) {
	store, _ := newTempStore(t)

	if err := store.AddPermanentRule(Rule{Path: "/p", Mode: ModeRead}); err != nil {
		t.Fatalf("add r: %v", err)
	}
	if err := store.AddPermanentRule(Rule{Path: "/p", Mode: ModeWrite}); err != nil {
		t.Fatalf("add w: %v", err)
	}

	snap := store.Snapshot()
	if len(snap.Rules) != 1 {
		t.Fatalf("same path should upsert to a single rule, got %d", len(snap.Rules))
	}
	if snap.Rules[0].Mode != ModeReadWrite {
		t.Errorf("r ∪ w should widen to rw, got %q", snap.Rules[0].Mode)
	}
}

func TestUpsertRule_HardlockNotOverwrittenByPermanent(t *testing.T) {
	store, _ := newTempStore(t)
	if err := store.AddHardlock("/locked"); err != nil {
		t.Fatalf("AddHardlock: %v", err)
	}
	// Attempt to overwrite the hardlock with a permanent allow on the same path.
	if err := store.AddPermanentRule(Rule{Path: "/locked", Mode: ModeReadWrite}); err != nil {
		t.Fatalf("AddPermanentRule: %v", err)
	}
	snap := store.Snapshot()
	if len(snap.Rules) != 1 {
		t.Fatalf("expected single rule, got %d (%+v)", len(snap.Rules), snap.Rules)
	}
	if snap.Rules[0].Scope != ScopeHardlock || snap.Rules[0].Mode != "deny" {
		t.Errorf("hardlock must survive a permanent overwrite attempt, got %+v", snap.Rules[0])
	}
}

func TestRemoveRule_PermanentDropped_HardlockProtected(t *testing.T) {
	store, _ := newTempStore(t)
	_ = store.AddPermanentRule(Rule{Path: "/perm", Mode: ModeRead})
	_ = store.AddHardlock("/lock")

	// removeHardlock=false: permanent removed, hardlock kept.
	if err := store.RemoveRule("/perm", false); err != nil {
		t.Fatalf("RemoveRule perm: %v", err)
	}
	if err := store.RemoveRule("/lock", false); err != nil {
		t.Fatalf("RemoveRule lock (no force): %v", err)
	}
	snap := store.Snapshot()
	if len(snap.Rules) != 1 || snap.Rules[0].Path != "/lock" {
		t.Fatalf("permanent should be gone, hardlock kept: %+v", snap.Rules)
	}

	// removeHardlock=true: hardlock can now be removed.
	if err := store.RemoveRule("/lock", true); err != nil {
		t.Fatalf("RemoveRule lock (force): %v", err)
	}
	if len(store.Snapshot().Rules) != 0 {
		t.Errorf("forced removal should drop the hardlock, got %+v", store.Snapshot().Rules)
	}
}

func TestRemoveRule_PersistsToDisk(t *testing.T) {
	store, path := newTempStore(t)
	_ = store.AddPermanentRule(Rule{Path: "/perm", Mode: ModeRead})
	if err := store.RemoveRule("/perm", false); err != nil {
		t.Fatalf("RemoveRule: %v", err)
	}
	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Snapshot().Rules) != 0 {
		t.Errorf("removal not persisted: %+v", reloaded.Snapshot().Rules)
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	store, _ := newTempStore(t)
	now := time.Now()
	_ = store.AddSessionRule(Rule{Path: "/a", Mode: ModeRead, SessionID: "live", ExpiresAt: now.Add(time.Hour)})
	_ = store.AddSessionRule(Rule{Path: "/b", Mode: ModeRead, SessionID: "expired", ExpiresAt: now.Add(-time.Hour)})
	_ = store.AddSessionRule(Rule{Path: "/c", Mode: ModeRead, SessionID: "killed", ExpiresAt: now.Add(time.Hour)})

	if err := store.PurgeExpiredSessions(now, map[string]bool{"killed": true}); err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	snap := store.Snapshot()
	if len(snap.SessionGrants) != 1 {
		t.Fatalf("expected only the live grant to remain, got %+v", snap.SessionGrants)
	}
	if snap.SessionGrants[0].SessionID != "live" {
		t.Errorf("wrong survivor: %+v", snap.SessionGrants[0])
	}
}

func TestPurgeExpiredSessions_NoChange_NoError(t *testing.T) {
	store, _ := newTempStore(t)
	now := time.Now()
	_ = store.AddSessionRule(Rule{Path: "/a", Mode: ModeRead, SessionID: "live", ExpiresAt: now.Add(time.Hour)})
	// Nothing expired or terminated — purge should be a no-op without error.
	if err := store.PurgeExpiredSessions(now, nil); err != nil {
		t.Fatalf("PurgeExpiredSessions no-op: %v", err)
	}
	if len(store.Snapshot().SessionGrants) != 1 {
		t.Errorf("no-op purge changed state: %+v", store.Snapshot().SessionGrants)
	}
}

func TestUnionMode(t *testing.T) {
	cases := []struct {
		a, b, want Mode
	}{
		{ModeRead, ModeWrite, ModeReadWrite},
		{ModeWrite, ModeRead, ModeReadWrite},
		{ModeRead, ModeReadWrite, ModeReadWrite},
		{ModeReadWrite, ModeWrite, ModeReadWrite},
		{ModeRead, ModeRead, ModeRead},
		{"deny", ModeReadWrite, "deny"},
		{ModeReadWrite, "deny", "deny"},
	}
	for _, c := range cases {
		if got := unionMode(c.a, c.b); got != c.want {
			t.Errorf("unionMode(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	store, _ := newTempStore(t)
	var wg sync.WaitGroup
	const workers = 8

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p := filepath.Join("/concurrent", string(rune('a'+id)))
			for j := 0; j < 20; j++ {
				_ = store.AddPermanentRule(Rule{Path: p, Mode: ModeRead})
				_ = store.Snapshot()
				_ = store.AddSessionRule(Rule{Path: p, Mode: ModeRead, SessionID: p, ExpiresAt: time.Now().Add(time.Hour)})
			}
		}(i)
	}
	wg.Wait()

	snap := store.Snapshot()
	if len(snap.Rules) != workers {
		t.Errorf("expected %d distinct permanent rules, got %d", workers, len(snap.Rules))
	}
}
