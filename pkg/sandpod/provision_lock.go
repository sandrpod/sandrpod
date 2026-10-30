// Copyright 2026 SandrPod Contributors
// One VM per provider and region at a time.
//
// A create that finds no eligible worker provisions a VM, and a VM takes minutes
// to become a worker. Two creates 90 seconds apart therefore both look at an empty
// fleet, both provision, and the customer pays for a machine nobody asked for —
// measured on a real deployment: two creates, two VMs, one of them idle forever
// until the idle reaper noticed.
//
// A reservation is a claim on "somebody is bringing a worker up for this provider and
// region". The second create waits for it instead of racing it. Three properties
// matter:
//
//   - it is a *wait*, not a refusal. If the first attempt dies, the second must still
//     get its sandbox, so the waiter falls through to provisioning after the deadline;
//   - it expires on its own. A process killed mid-provision must not leave a
//     reservation that blocks every later create for that region;
//   - it is keyed by provider and region, never by sandbox name: the point is to
//     share one machine, which means sharing it across different creates.
package sandpod

import (
	"sync"
	"time"
)

// provisioningTTL bounds how long a reservation is believed. A VM that has not
// registered in this long is not going to serve this create, and the waiter
// provisions for itself rather than waiting forever.
const provisioningTTL = 8 * time.Minute

type provisionBook struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

var provisioning = &provisionBook{seen: map[string]time.Time{}}

// reserveProvision claims the provider+region for one provisioning attempt. It
// returns true for the caller that should provision, false for a caller that should
// wait for the attempt already underway.
func (b *provisionBook) reserve(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if at, ok := b.seen[key]; ok && now.Sub(at) < provisioningTTL {
		return false
	}
	b.seen[key] = now
	return true
}

// release drops the claim, so a create that provisions *and fails* does not make
// the next one wait for nothing.
func (b *provisionBook) release(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.seen, key)
}

// provisioningKey is what "same machine" means: the provider and the region. A
// container-based worker (local) never provisions and never reaches this.
func provisioningKey(providerType, region string) string {
	return providerType + "|" + region
}

// provisioningInFlight reports whether someone is bringing a worker up, without
// claiming anything. Used to decide whether waiting is worthwhile at all.
func (b *provisionBook) inFlight(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.seen[key]
	return ok && time.Since(at) < provisioningTTL
}
