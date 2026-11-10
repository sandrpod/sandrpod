// Copyright 2026 SandrPod Contributors
// Idle reclamation: opt-in reapers for idle sandboxes and empty cloud poders.
//
// Both are cost-safety features — a platform that provisions cloud VMs on
// behalf of users must be able to give them back. Both default to OFF so
// existing deployments upgrade with no behavior change.

package server

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
func reapIdleSandboxes(ctx context.Context, ttl, retain time.Duration, ss podpkg.SandboxRepository, ps podpkg.PoderRepository, ts *tunnel.TunnelStore) {
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
			// A stopped sandbox past its retention window is finally removed.
			// Its workspace volume goes with an explicit delete; the data is
			// meant to outlive the container, not the sandbox.
			if sb.State == podpkg.StateStopped && stoppedTooLong(sb, now, retain) {
				log.Printf("idle reaper: deleting sandbox %s (stopped %v > retention %v)", sb.Name, now.Sub(sb.LastActivity).Round(time.Minute), retain)
				notifyEvent("sandbox.reaped", sandboxEvent(sb, map[string]any{"reason": "retention_expired"}))
				teardownSandbox(ctx, sb, ss, ps, ts)
				continue
			}
			if !shouldReapSandbox(sb, now, ttl) {
				continue
			}
			// Idle: release the container's CPU and memory but keep the sandbox
			// and its data. `start` resumes it (docker start, volume intact).
			log.Printf("idle reaper: stopping sandbox %s (idle, ttl %v)", sb.Name, effectiveSandboxTTL(sb, ttl))
			notifyEvent("sandbox.reaped", sandboxEvent(sb, map[string]any{"reason": "ttl_idle_stop"}))
			stopSandbox(ctx, sb, ss, ts)
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
	effective := effectiveSandboxTTL(sb, ttl)
	if effective <= 0 {
		return false
	}
	last := sb.LastActivity
	if last.IsZero() {
		last = sb.CreatedAt
	}
	return !last.IsZero() && now.Sub(last) > effective
}

// effectiveSandboxTTL is the idle TTL actually applied to a sandbox: its own
// TTLSeconds when set (e.g. an E2B `Sandbox.create(timeout=…)`), otherwise the
// server-wide default.
func effectiveSandboxTTL(sb *podpkg.SandboxInfo, ttl time.Duration) time.Duration {
	if sb.TTLSeconds > 0 {
		return time.Duration(sb.TTLSeconds) * time.Second
	}
	return ttl
}

// stopSandbox releases the container's resources without destroying anything:
// the pode stops the container (docker stop), the workspace volume stays, and
// the record moves to STOPPED so `start` can resume it.
func stopSandbox(ctx context.Context, sb *podpkg.SandboxInfo, ss podpkg.SandboxRepository, ts *tunnel.TunnelStore) {
	t, ok := ts.Get(sb.PoderID)
	if !ok {
		log.Printf("idle reaper: sandbox %s has no tunnel, cannot stop it now", sb.Name)
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, "http://poder/sandboxes/"+sb.Name+"/stop", nil)
	resp, err := t.Client.Do(req)
	if err != nil {
		log.Printf("idle reaper: stop %s failed: %v", sb.Name, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("idle reaper: stop %s failed: HTTP %d", sb.Name, resp.StatusCode)
		return
	}
	_ = ss.Update(sb.Name, func(s *podpkg.SandboxInfo) { s.State = podpkg.StateStopped })
	log.Printf("idle reaper: sandbox %s stopped, data kept in its workspace volume", sb.Name)
}

// stoppedTooLong reports whether a STOPPED sandbox has outlived the retention
// window. LastActivity is refreshed on every use, so a resumed sandbox starts a
// new clock.
func stoppedTooLong(sb *podpkg.SandboxInfo, now time.Time, retain time.Duration) bool {
	if retain <= 0 {
		return false
	}
	last := sb.LastActivity
	if last.IsZero() {
		last = sb.CreatedAt
	}
	return now.Sub(last) > retain
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
		// The sandbox records decide emptiness, not the container counter. A
		// container whose record is gone is an orphan: nothing can reach it, and
		// it must not pin a metered VM alive forever. That happened — a create
		// that timed out left a container behind, usage stayed at 1, and the VM
		// outlived its purpose by however long nobody looked.
		if len(ss.ListByPoderID(p.ID)) > 0 {
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
