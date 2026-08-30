package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/e2bcompat"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

// -max-sandboxes-per-owner was enforced only in the native POST
// /api/v1/sandboxes handler, so it did nothing at all on the E2B surface —
// the one most deployments put on a public IP. A user token could create
// sandboxes until the substrate ran out.
func TestE2BCreateSandbox_EnforcesPerOwnerQuota(t *testing.T) {
	prev := podpkg.LocalPoderGracePeriod
	podpkg.LocalPoderGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { podpkg.LocalPoderGracePeriod = prev })

	stores := store.NewMemoryStores()
	for i := 0; i < 2; i++ {
		if err := stores.Sandboxes.Add(&podpkg.SandboxInfo{
			Name: fmt.Sprintf("e2b%d", i), Owner: "alice", State: podpkg.StateRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One sandbox owned by someone else must not count against alice.
	if err := stores.Sandboxes.Add(&podpkg.SandboxInfo{
		Name: "e2b-bob", Owner: "bob", State: podpkg.StateRunning,
	}); err != nil {
		t.Fatal(err)
	}

	b := &e2bSandboxBackend{e2bDeps{
		cfg:       serverConfig{MaxSandboxesPerOwner: 2},
		scheduler: podpkg.NewScheduler(stores.Poders, "", ""),
		sandboxes: stores.Sandboxes,
		poders:    stores.Poders,
		jobs:      stores.Jobs,
	}}

	_, err := b.CreateSandbox("alice", false, e2bcompat.NewSandbox{})
	if !errors.Is(err, e2bcompat.ErrQuotaExceeded) {
		t.Fatalf("at the cap the E2B create path returned %v, want ErrQuotaExceeded", err)
	}

	// bob holds one of three; the cap is per owner, not global.
	if _, err := b.CreateSandbox("bob", false, e2bcompat.NewSandbox{}); errors.Is(err, e2bcompat.ErrQuotaExceeded) {
		t.Errorf("bob holds 1 of a cap of 2 but was refused — the count is not per owner")
	}
}

// Admins are exempt on the native surface; the E2B surface must agree, or
// pointing the SDK at an admin token starts failing at the user cap.
func TestE2BCreateSandbox_AdminExemptFromQuota(t *testing.T) {
	prev := podpkg.LocalPoderGracePeriod
	podpkg.LocalPoderGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { podpkg.LocalPoderGracePeriod = prev })

	stores := store.NewMemoryStores()
	for i := 0; i < 5; i++ {
		if err := stores.Sandboxes.Add(&podpkg.SandboxInfo{
			Name: fmt.Sprintf("e2b%d", i), Owner: "root", State: podpkg.StateRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	b := &e2bSandboxBackend{e2bDeps{
		cfg:       serverConfig{MaxSandboxesPerOwner: 1},
		scheduler: podpkg.NewScheduler(stores.Poders, "", ""),
		sandboxes: stores.Sandboxes,
		poders:    stores.Poders,
		jobs:      stores.Jobs,
	}}

	// No poder is registered, so this still fails — but it must get past the
	// quota gate to do so.
	_, err := b.CreateSandbox("root", true, e2bcompat.NewSandbox{})
	if errors.Is(err, e2bcompat.ErrQuotaExceeded) {
		t.Fatalf("an admin was refused by the per-owner quota: %v", err)
	}
}

func TestQuotaExceeded_ZeroMeansUnlimited(t *testing.T) {
	stores := store.NewMemoryStores()
	for i := 0; i < 50; i++ {
		if err := stores.Sandboxes.Add(&podpkg.SandboxInfo{
			Name: fmt.Sprintf("s%d", i), Owner: "alice", State: podpkg.StateRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if quotaExceeded(stores.Sandboxes, "alice", 0) {
		t.Error("max=0 must mean unlimited, not a cap of zero")
	}
	if !quotaExceeded(stores.Sandboxes, "alice", 50) {
		t.Error("50 held against a cap of 50 must be over")
	}
}
