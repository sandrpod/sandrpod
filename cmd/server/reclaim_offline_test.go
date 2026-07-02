package main

import (
	"context"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

// offlineFixture registers a poder, marks it OFFLINE, and returns the store plus
// a `now` far enough past its heartbeat to be reapable.
func offlineFixture(t *testing.T, id, providerType, vmID string) (podpkg.PoderRepository, time.Time, time.Duration) {
	t.Helper()
	stores := store.NewMemoryStores()
	base := time.Now()
	if _, err := stores.Poders.Register(&podpkg.RegisterPoderRequest{
		ID: id, ProviderType: providerType, VMID: vmID,
	}); err != nil {
		t.Fatal(err)
	}
	stores.Poders.SetOffline(id)
	timeout := 10 * time.Minute
	return stores.Poders, base.Add(2 * timeout), timeout // now = base + 20min, LastHeartbeat ≈ base
}

// A local/docker poder that goes OFFLINE past the timeout has its stale record
// cleaned up — but NO VM-terminate is attempted (it has none).
func TestReapOfflinePodersOnce_LocalRecordOnly(t *testing.T) {
	ps, now, timeout := offlineFixture(t, "off-local", "docker", "")
	terminate := func(context.Context, string, string) error {
		t.Fatal("local poder must never hit the VM terminator")
		return nil
	}
	reapOfflinePodersOnce(context.Background(), now, timeout, ps, terminate)
	if _, ok := ps.Get("off-local"); ok {
		t.Error("stale OFFLINE local poder record should be cleaned up")
	}
	if !poderTombstones.Contains("off-local") {
		t.Error("reaped poder should be tombstoned")
	}
}

// An ONLINE (healthy, possibly idle) local poder is never touched by the
// offline reaper, no matter how old its last heartbeat looks.
func TestReapOfflinePodersOnce_OnlineLocalSpared(t *testing.T) {
	stores := store.NewMemoryStores()
	_, _ = stores.Poders.Register(&podpkg.RegisterPoderRequest{ID: "on-local", ProviderType: "docker"})
	// deliberately NOT SetOffline → stays ONLINE
	terminate := func(context.Context, string, string) error {
		t.Fatal("online poder must never be reaped")
		return nil
	}
	reapOfflinePodersOnce(context.Background(), time.Now().Add(time.Hour), 10*time.Minute, stores.Poders, terminate)
	if _, ok := stores.Poders.Get("on-local"); !ok {
		t.Error("ONLINE local poder must survive the offline reaper")
	}
}

// A cloud poder that goes OFFLINE past the timeout gets its VM terminated,
// then the record deleted + tombstoned.
func TestReapOfflinePodersOnce_CloudTerminatesVM(t *testing.T) {
	ps, now, timeout := offlineFixture(t, "off-cloud", "gcp", "vm-off-1")
	var terminated []string
	terminate := func(_ context.Context, pt, vm string) error {
		if pt != "gcp" || vm != "vm-off-1" {
			t.Errorf("terminate(%q,%q), want (gcp,vm-off-1)", pt, vm)
		}
		terminated = append(terminated, vm)
		return nil
	}
	reapOfflinePodersOnce(context.Background(), now, timeout, ps, terminate)
	if len(terminated) != 1 {
		t.Fatalf("cloud VM not terminated exactly once: %v", terminated)
	}
	if _, ok := ps.Get("off-cloud"); ok {
		t.Error("cloud poder record should be deleted after VM terminate")
	}
}

// OFFLINE but still within the timeout window → not yet reaped.
func TestReapOfflinePodersOnce_WithinTimeoutSpared(t *testing.T) {
	stores := store.NewMemoryStores()
	base := time.Now()
	_, _ = stores.Poders.Register(&podpkg.RegisterPoderRequest{ID: "off-fresh", ProviderType: "docker"})
	stores.Poders.SetOffline("off-fresh")
	terminate := func(context.Context, string, string) error { return nil }
	// now only 1 minute past heartbeat, timeout is 10 minutes
	reapOfflinePodersOnce(context.Background(), base.Add(time.Minute), 10*time.Minute, stores.Poders, terminate)
	if _, ok := stores.Poders.Get("off-fresh"); !ok {
		t.Error("poder OFFLINE within the timeout must not be reaped yet")
	}
}
