// Copyright 2024 SandrPod
// Idle reclamation: opt-in reapers for idle sandboxes and empty cloud poders.
//
// Both are cost-safety features — a platform that provisions cloud VMs on
// behalf of users must be able to give them back. Both default to OFF so
// existing deployments upgrade with no behavior change.

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// reapIdleSandboxes deletes sandboxes whose last activity is older than ttl.
// Activity is touched on every tunnel use (execute/stream/session/toolbox/
// start/stop) via sandboxTunnel. Direct-agent sandboxes (a user's own machine,
// no cloud cost) are never reaped. Records created before the upgrade have a
// zero LastActivity — CreatedAt is used as the fallback baseline.
func reapIdleSandboxes(ctx context.Context, ttl time.Duration, ss podpkg.SandboxRepository, ps podpkg.PoderRepository, ts *tunnel.TunnelStore) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		for _, sb := range ss.List() {
			if !shouldReapSandbox(sb, now, ttl) {
				continue
			}
			log.Printf("idle reaper: deleting sandbox %s (idle, ttl %v)", sb.Name, ttl)
			notifyEvent("sandbox.reaped", map[string]any{"name": sb.Name, "provider": sb.ProviderType})
			teardownSandbox(ctx, sb, ss, ps, ts)
		}
	}
}

// shouldReapSandbox reports whether a sandbox is past the idle TTL and safe to
// reap. Direct-agent sandboxes (a user's own machine, no cloud cost) and
// still-provisioning states are never reaped; records without a LastActivity
// fall back to CreatedAt.
func shouldReapSandbox(sb *podpkg.SandboxInfo, now time.Time, ttl time.Duration) bool {
	if strings.HasPrefix(sb.ProxyURL, "direct://") {
		return false
	}
	switch sb.State {
	case podpkg.StateRunning, podpkg.StateStopped, podpkg.StateError:
	default:
		return false // PENDING/STARTING etc. are still provisioning
	}
	// A per-sandbox TTL overrides the global default; with neither set the
	// sandbox is never reaped.
	effective := ttl
	if sb.TTLSeconds > 0 {
		effective = time.Duration(sb.TTLSeconds) * time.Second
	}
	if effective <= 0 {
		return false
	}
	last := sb.LastActivity
	if last.IsZero() {
		last = sb.CreatedAt
	}
	return !last.IsZero() && now.Sub(last) > effective
}

// teardownSandbox removes a sandbox record, decrements its poder's usage, and
// best-effort deletes the container over the tunnel — the same steps as the
// DELETE handler.
func teardownSandbox(ctx context.Context, sb *podpkg.SandboxInfo, ss podpkg.SandboxRepository, ps podpkg.PoderRepository, ts *tunnel.TunnelStore) {
	_ = ss.Delete(sb.Name)
	ps.UpdateUsage(sb.PoderID, func(u *podpkg.PoderUsage) {
		if u.Containers > 0 {
			u.Containers--
		}
	})
	if t, ok := ts.Get(sb.PoderID); ok {
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodDelete, "http://poder/sandboxes/"+sb.Name, nil)
		if resp, err := t.Client.Do(req); err != nil {
			log.Printf("idle reaper: container cleanup for %s failed: %v", sb.Name, err)
		} else {
			resp.Body.Close()
		}
		cancel()
	}
}

// reapIdlePoders reclaims ONLINE cloud poders that have had zero containers
// (and zero sandbox records) for longer than ttl: the VM is terminated, the
// poder tombstoned and its record deleted. Empty-since tracking is in-memory;
// a server restart just restarts the clock, which only delays reclamation.
// vmTerminator terminates the cloud VM behind a poder. Injectable so the reap
// loop can be unit-tested without the real provider factory / cloud APIs.
type vmTerminator func(ctx context.Context, providerType, vmID string) error

// factoryVMTerminator is the production terminator: look the provider up in the
// global factory and call DeleteVM with a bounded context.
func factoryVMTerminator(_ context.Context, providerType, vmID string) error {
	prov, err := provider.GetFactory().Get(providerType)
	if err != nil {
		return fmt.Errorf("provider %q unavailable: %w", providerType, err)
	}
	delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return prov.DeleteVM(delCtx, vmID)
}

func reapIdlePoders(ctx context.Context, ttl time.Duration, ps podpkg.PoderRepository, ss podpkg.SandboxRepository, ts *tunnel.TunnelStore) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	emptySince := make(map[string]time.Time)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		reapEmptyPodersOnce(ctx, time.Now(), ttl, ps, ss, ts, emptySince, factoryVMTerminator)
	}
}

// reapEmptyPodersOnce runs one scan pass: it advances the emptySince tracking
// map and reclaims any ONLINE cloud poder that has had zero containers (and zero
// sandbox records) for longer than ttl — terminating the VM, tombstoning the
// poder ID, closing its tunnel, and deleting the record. Pure w.r.t. time (now
// is passed in) and the cloud API (terminate is injected), so it is directly
// unit-testable.
func reapEmptyPodersOnce(
	ctx context.Context,
	now time.Time,
	ttl time.Duration,
	ps podpkg.PoderRepository,
	ss podpkg.SandboxRepository,
	ts *tunnel.TunnelStore,
	emptySince map[string]time.Time,
	terminate vmTerminator,
) {
	live := make(map[string]bool)
	for _, p := range ps.List() {
		live[p.ID] = true
		if p.State != podpkg.PoderStateOnline || !isCloudProvider(p.ProviderType) || p.VMID == "" {
			delete(emptySince, p.ID)
			continue
		}
		if p.Usage.Containers > 0 || len(ss.ListByPoderID(p.ID)) > 0 {
			delete(emptySince, p.ID)
			continue
		}
		since, seen := emptySince[p.ID]
		if !seen {
			emptySince[p.ID] = now
			continue
		}
		if now.Sub(since) <= ttl {
			continue
		}
		if err := terminate(ctx, p.ProviderType, p.VMID); err != nil {
			log.Printf("idle reaper: poder %s VM %s termination failed, retrying next tick: %v", p.ID, p.VMID, err)
			continue
		}
		poderTombstones.Add(p.ID)
		if t, ok := ts.Get(p.ID); ok {
			t.Close()
		}
		if err := ps.Delete(p.ID); err != nil {
			log.Printf("idle reaper: poder %s record delete failed: %v", p.ID, err)
			continue
		}
		delete(emptySince, p.ID)
		log.Printf("idle reaper: reclaimed poder %s (empty %v > %v), VM %s terminated", p.ID, now.Sub(since).Round(time.Second), ttl, p.VMID)
		notifyEvent("poder.reclaimed", map[string]any{"poder_id": p.ID, "vm_id": p.VMID, "provider": p.ProviderType})
	}
	// Drop tracking for poders that no longer exist.
	for id := range emptySince {
		if !live[id] {
			delete(emptySince, id)
		}
	}
}
