// Copyright 2026 SandrPod Contributors

package main

import (
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// reconcileByHeartbeat is now the authoritative reconcile path.
// These tests verify that sandbox states are updated correctly based on the
// container-name list carried in a Poder heartbeat.

func addSandbox(store *podpkg.SandboxStore, name, poderID string, state podpkg.State) {
	_ = store.Add(&podpkg.SandboxInfo{
		Name:    name,
		PoderID: poderID,
		State:   state,
	})
}

func TestReconcileByHeartbeat_MarksGoneAsError(t *testing.T) {
	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-alive", "fake-poder", podpkg.StateRunning)
	addSandbox(store, "sb-gone", "fake-poder", podpkg.StateRunning)

	// Heartbeat says only sb-alive is running.
	reconcileByHeartbeat("fake-poder", []string{"sb-alive"}, store)

	if sb, _ := store.Get("sb-alive"); sb.State != podpkg.StateRunning {
		t.Errorf("sb-alive: expected RUNNING, got %s", sb.State)
	}
	if sb, _ := store.Get("sb-gone"); sb.State != podpkg.StateError {
		t.Errorf("sb-gone: expected ERROR, got %s", sb.State)
	}
}

func TestReconcileByHeartbeat_SkipsNonRunning(t *testing.T) {
	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-stopped", "fake-poder", podpkg.StateStopped)

	// Empty container list — nothing to reconcile for non-running sandboxes.
	reconcileByHeartbeat("fake-poder", []string{}, store)

	if sb, _ := store.Get("sb-stopped"); sb.State != podpkg.StateStopped {
		t.Errorf("sb-stopped: expected STOPPED unchanged, got %s", sb.State)
	}
}

func TestReconcileByHeartbeat_AllAlive(t *testing.T) {
	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-a", "fake-poder", podpkg.StateRunning)
	addSandbox(store, "sb-b", "fake-poder", podpkg.StateRunning)

	reconcileByHeartbeat("fake-poder", []string{"sb-a", "sb-b"}, store)

	for _, name := range []string{"sb-a", "sb-b"} {
		if sb, _ := store.Get(name); sb.State != podpkg.StateRunning {
			t.Errorf("%s: expected RUNNING, got %s", name, sb.State)
		}
	}
}

func TestReconcileByHeartbeat_EmptyListMarksAllError(t *testing.T) {
	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-a", "fake-poder", podpkg.StateRunning)
	addSandbox(store, "sb-b", "fake-poder", podpkg.StateStarting)

	// Empty (non-nil) list means no containers are alive.
	reconcileByHeartbeat("fake-poder", []string{}, store)

	for _, name := range []string{"sb-a", "sb-b"} {
		if sb, _ := store.Get(name); sb.State != podpkg.StateError {
			t.Errorf("%s: expected ERROR, got %s", name, sb.State)
		}
	}
}

func TestReconcileByHeartbeat_RestoresErrorIfContainerAlive(t *testing.T) {
	store := podpkg.NewSandboxStore()
	// Simulates: disconnect handler marked sandbox ERROR, but container is still running.
	addSandbox(store, "sb-recover", "fake-poder", podpkg.StateError)
	addSandbox(store, "sb-truly-gone", "fake-poder", podpkg.StateError)

	// Heartbeat: only sb-recover is alive.
	reconcileByHeartbeat("fake-poder", []string{"sb-recover"}, store)

	if sb, _ := store.Get("sb-recover"); sb.State != podpkg.StateRunning {
		t.Errorf("sb-recover: expected RUNNING (restored), got %s", sb.State)
	}
	if sb, _ := store.Get("sb-truly-gone"); sb.State != podpkg.StateError {
		t.Errorf("sb-truly-gone: expected ERROR (unchanged), got %s", sb.State)
	}
}

func TestReconcileByHeartbeat_OnlyOwnPoderReconciled(t *testing.T) {
	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-mine", "poder-A", podpkg.StateRunning)
	addSandbox(store, "sb-other", "poder-B", podpkg.StateRunning)

	// Reconcile only poder-A with an empty list.
	reconcileByHeartbeat("poder-A", []string{}, store)

	if sb, _ := store.Get("sb-mine"); sb.State != podpkg.StateError {
		t.Errorf("sb-mine: expected ERROR, got %s", sb.State)
	}
	// poder-B's sandbox must not be touched.
	if sb, _ := store.Get("sb-other"); sb.State != podpkg.StateRunning {
		t.Errorf("sb-other: expected RUNNING (untouched), got %s", sb.State)
	}
}
