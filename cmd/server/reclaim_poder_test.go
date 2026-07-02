package main

import (
	"context"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// Exercises the full empty-poder reclaim chain deterministically:
// empty-since tracking → TTL expiry → VM terminate → tombstone → record delete.
func TestReapEmptyPodersOnce_Chain(t *testing.T) {
	stores := store.NewMemoryStores()
	ts := tunnel.NewTunnelStore()
	const pid, vmID = "reap-chain-poder", "vm-abc-123"

	if _, err := stores.Poders.Register(&podpkg.RegisterPoderRequest{
		ID:           pid,
		ProviderType: "gcp",
		VMID:         vmID,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	var terminated []string
	terminate := func(_ context.Context, providerType, id string) error {
		if providerType != "gcp" || id != vmID {
			t.Errorf("terminate called with (%q,%q), want (gcp,%q)", providerType, id, vmID)
		}
		terminated = append(terminated, id)
		return nil
	}

	emptySince := map[string]time.Time{}
	ttl := 60 * time.Second
	t0 := time.Now()

	// Pass 1: first time seen empty → records emptySince, no reap.
	reapEmptyPodersOnce(context.Background(), t0, ttl, stores.Poders, stores.Sandboxes, ts, emptySince, terminate)
	if len(terminated) != 0 {
		t.Fatalf("pass1 reaped too early: %v", terminated)
	}
	if _, ok := stores.Poders.Get(pid); !ok {
		t.Fatal("pass1 deleted the poder prematurely")
	}
	if _, seen := emptySince[pid]; !seen {
		t.Fatal("pass1 did not start empty-since tracking")
	}

	// Pass 2: still within TTL → no reap.
	reapEmptyPodersOnce(context.Background(), t0.Add(30*time.Second), ttl, stores.Poders, stores.Sandboxes, ts, emptySince, terminate)
	if len(terminated) != 0 {
		t.Fatalf("pass2 reaped within TTL: %v", terminated)
	}

	// Pass 3: past TTL → reap: terminate VM, tombstone, delete record.
	reapEmptyPodersOnce(context.Background(), t0.Add(90*time.Second), ttl, stores.Poders, stores.Sandboxes, ts, emptySince, terminate)
	if len(terminated) != 1 {
		t.Fatalf("pass3 did not terminate the VM exactly once: %v", terminated)
	}
	if _, ok := stores.Poders.Get(pid); ok {
		t.Error("pass3 did not delete the poder record")
	}
	if !poderTombstones.Contains(pid) {
		t.Error("pass3 did not tombstone the poder id (would allow ghost re-registration)")
	}
	if _, seen := emptySince[pid]; seen {
		t.Error("pass3 left stale empty-since tracking")
	}
}

// A poder that still has a container/sandbox is never reaped, and its
// empty-since tracking is cleared.
func TestReapEmptyPodersOnce_BusyPoderSpared(t *testing.T) {
	stores := store.NewMemoryStores()
	ts := tunnel.NewTunnelStore()
	const pid = "reap-busy-poder"
	_, _ = stores.Poders.Register(&podpkg.RegisterPoderRequest{ID: pid, ProviderType: "gcp", VMID: "vm-busy"})
	_ = stores.Poders.UpdateUsage(pid, func(u *podpkg.PoderUsage) { u.Containers = 1 })

	terminate := func(context.Context, string, string) error {
		t.Fatal("busy poder must never be terminated")
		return nil
	}
	emptySince := map[string]time.Time{pid: time.Now().Add(-time.Hour)} // pretend previously empty
	reapEmptyPodersOnce(context.Background(), time.Now(), time.Second, stores.Poders, stores.Sandboxes, ts, emptySince, terminate)
	if _, seen := emptySince[pid]; seen {
		t.Error("busy poder should have its empty-since tracking cleared")
	}
	if _, ok := stores.Poders.Get(pid); !ok {
		t.Error("busy poder must not be deleted")
	}
}

// Local/docker poders (no VM) are never touched by the cloud reaper.
func TestReapEmptyPodersOnce_LocalPoderIgnored(t *testing.T) {
	stores := store.NewMemoryStores()
	ts := tunnel.NewTunnelStore()
	const pid = "reap-local-poder"
	_, _ = stores.Poders.Register(&podpkg.RegisterPoderRequest{ID: pid, ProviderType: "docker", VMID: ""})

	terminate := func(context.Context, string, string) error {
		t.Fatal("local poder must never hit the terminator")
		return nil
	}
	emptySince := map[string]time.Time{}
	for i := range 3 {
		reapEmptyPodersOnce(context.Background(), time.Now().Add(time.Duration(i)*time.Hour), time.Second, stores.Poders, stores.Sandboxes, ts, emptySince, terminate)
	}
	if _, ok := stores.Poders.Get(pid); !ok {
		t.Error("local poder must survive")
	}
}
