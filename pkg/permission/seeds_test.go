// Copyright 2026 SandrPod
// Tests for first-run seeding of hardlocks and command policy.

package permission

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultHardlockSeeds_AllHardlockDeny(t *testing.T) {
	seeds := DefaultHardlockSeeds()
	if len(seeds) == 0 {
		t.Fatal("expected a non-empty seed list")
	}
	for _, r := range seeds {
		if r.Scope != ScopeHardlock {
			t.Errorf("seed %q has scope %q, want hardlock", r.Path, r.Scope)
		}
		if r.Mode != "deny" {
			t.Errorf("seed %q has mode %q, want deny", r.Path, r.Mode)
		}
		if r.Path == "" {
			t.Error("seed has empty path")
		}
	}

	// Common cross-platform entries must always be present.
	wantCommon := []string{"~/.ssh", "~/.aws", "~/.gnupg"}
	have := make(map[string]bool, len(seeds))
	for _, r := range seeds {
		have[r.Path] = true
	}
	for _, w := range wantCommon {
		if !have[w] {
			t.Errorf("expected common seed %q to be present", w)
		}
	}

	// At least one OS-specific entry on the major platforms.
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		if len(seeds) <= len(wantCommon) {
			t.Errorf("expected OS-specific seeds on %s, only got %d", runtime.GOOS, len(seeds))
		}
	}
}

func TestSeedHardlocksIfEmpty_SeedsOnlyWhenEmpty(t *testing.T) {
	store, _ := newTempStore(t)

	added, err := SeedHardlocksIfEmpty(store)
	if err != nil {
		t.Fatalf("SeedHardlocksIfEmpty: %v", err)
	}
	if added == 0 {
		t.Fatal("expected seeds to be added to an empty store")
	}
	if got := len(store.Snapshot().Rules); got != added {
		t.Errorf("store has %d rules, seeder reported %d added", got, added)
	}

	// Second invocation must be a no-op (store is no longer empty).
	added2, err := SeedHardlocksIfEmpty(store)
	if err != nil {
		t.Fatalf("second SeedHardlocksIfEmpty: %v", err)
	}
	if added2 != 0 {
		t.Errorf("seeding a non-empty store should add 0, got %d", added2)
	}
	if got := len(store.Snapshot().Rules); got != added {
		t.Errorf("rule count changed on second seed: %d", got)
	}
}

func TestSeedHardlocksIfEmpty_NonEmptyStoreUntouched(t *testing.T) {
	store, _ := newTempStore(t)
	_ = store.AddPermanentRule(Rule{Path: "/Users/test/Documents", Mode: ModeRead})

	added, err := SeedHardlocksIfEmpty(store)
	if err != nil {
		t.Fatalf("SeedHardlocksIfEmpty: %v", err)
	}
	if added != 0 {
		t.Errorf("must not seed when any rule already exists, added %d", added)
	}
	if len(store.Snapshot().Rules) != 1 {
		t.Errorf("existing rules disturbed: %+v", store.Snapshot().Rules)
	}
}

func TestSeedHardlocks_PersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	added, err := SeedHardlocksIfEmpty(store)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(reloaded.Snapshot().Rules); got != added {
		t.Errorf("seeded rules not persisted: reloaded %d, seeded %d", got, added)
	}
}

func TestDefaultCommandPolicy_NonEmpty(t *testing.T) {
	p := DefaultCommandPolicy()
	if len(p.Deny) == 0 {
		t.Error("default policy should deny some commands")
	}
	if len(p.Warn) == 0 {
		t.Error("default policy should warn on some commands")
	}
	// Spot-check a few well-known entries.
	denySet := make(map[string]bool, len(p.Deny))
	for _, d := range p.Deny {
		denySet[d] = true
	}
	for _, want := range []string{"scp", "nc", "sudo"} {
		if !denySet[want] {
			t.Errorf("default deny list missing %q", want)
		}
	}
}

func TestSeedCommandPolicyIfEmpty_OnlyWhenEmpty(t *testing.T) {
	store, _ := newTempStore(t)

	added, err := SeedCommandPolicyIfEmpty(store)
	if err != nil {
		t.Fatalf("SeedCommandPolicyIfEmpty: %v", err)
	}
	if !added {
		t.Fatal("expected policy to be seeded into empty store")
	}
	snap := store.Snapshot()
	if len(snap.CommandPolicy.Deny) == 0 {
		t.Error("deny list should be populated after seeding")
	}

	// Second call is a no-op.
	added2, err := SeedCommandPolicyIfEmpty(store)
	if err != nil {
		t.Fatalf("second SeedCommandPolicyIfEmpty: %v", err)
	}
	if added2 {
		t.Error("seeding a populated policy should be a no-op")
	}
}

func TestSeedCommandPolicyIfEmpty_NotOverwritten(t *testing.T) {
	store, _ := newTempStore(t)
	_ = store.SetCommandPolicy(CommandPolicy{Deny: []string{"custom-tool"}})

	added, err := SeedCommandPolicyIfEmpty(store)
	if err != nil {
		t.Fatalf("SeedCommandPolicyIfEmpty: %v", err)
	}
	if added {
		t.Error("must not seed over an existing policy")
	}
	snap := store.Snapshot()
	if len(snap.CommandPolicy.Deny) != 1 || snap.CommandPolicy.Deny[0] != "custom-tool" {
		t.Errorf("existing policy clobbered: %+v", snap.CommandPolicy)
	}
}
