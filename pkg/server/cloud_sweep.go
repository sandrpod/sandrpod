// Copyright 2026 SandrPod Contributors
// Cloud-side reconciliation: find VMs SandrPod created that no poder record
// owns, and release them.
//
// The in-store reapers (reclaim.go) walk poder rows, so a VM whose poder record
// was deleted — or never written because the create failed halfway — is
// invisible to them and bills forever. This sweeper goes the other way: ask each
// provider what exists, and treat a SandrPod-named VM with no owning poder as
// an orphan.

package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sandrpod/sandrpod/pkg/provider"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// cloudSweepMode decides what an orphan sweep does.
type cloudSweepMode string

const (
	sweepOff    cloudSweepMode = "off"
	sweepAlert  cloudSweepMode = "alert"
	sweepDelete cloudSweepMode = "delete"
)

// providerVM pairs a VM with the provider that reported it (VMInfo does not
// carry the provider name).
type providerVM struct {
	Provider string
	VM       *provider.VMInfo
}

// vmLister is the provider surface the sweep needs; a provider with no ListVMs
// support can return an error and the sweep skips it.
type vmLister func(ctx context.Context) ([]providerVM, error)

// startCloudSweeper launches the sweep loop when configured. Off by default:
// deleting cloud resources on a timer is not something a self-hoster should get
// by accident.
func startCloudSweeper(ctx context.Context, ps podpkg.PoderRepository) {
	mode := cloudSweepMode(strings.ToLower(strings.TrimSpace(os.Getenv("SANDRPOD_CLOUD_SWEEP"))))
	if mode == "" {
		mode = sweepOff
	}
	if mode == sweepOff {
		return
	}
	if mode != sweepAlert && mode != sweepDelete {
		log.Printf("cloud sweeper: unknown SANDRPOD_CLOUD_SWEEP=%q, staying off (use alert|delete)", mode)
		return
	}
	interval := time.Hour
	if v := os.Getenv("SANDRPOD_CLOUD_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	log.Printf("cloud sweeper enabled: mode=%s interval=%v", mode, interval)
	go func() {
		// A first pass shortly after boot catches leftovers from a previous
		// process, then settle into the interval.
		time.Sleep(2 * time.Minute)
		for {
			sweepCloudOnce(ctx, ps, mode, providerLister, factoryVMTerminator)
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// providerLister asks every registered provider for its VMs. One provider
// failing does not hide the others.
func providerLister(ctx context.Context) ([]providerVM, error) {
	var out []providerVM
	var errs []string
	for _, name := range provider.GetFactory().Names() {
		p, err := provider.GetFactory().Get(name)
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		vms, err := p.ListVMs(ctx)
		cancel()
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		for _, vm := range vms {
			out = append(out, providerVM{Provider: name, VM: vm})
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

// sandrpodVMName reports whether the VM was provisioned by the scheduler, which
// names instances "sandrpod-<sandbox>". Tags are not part of VMInfo across
// providers, and every provider we support keeps the name.
func sandrpodVMName(name string) bool {
	return strings.HasPrefix(name, "sandrpod-")
}

// sweepCloudOnce runs one reconciliation pass. Pure w.r.t. the cloud API and the
// store (both injected), so it is directly unit-testable.
//
// Ownership is decided by VM id: any poder row (whatever its state) referencing
// the VM means somebody knows about it, and the sweep leaves it alone. That
// matters because a VM mid-handoff can legitimately have a younger row.
func sweepCloudOnce(
	ctx context.Context,
	ps podpkg.PoderRepository,
	mode cloudSweepMode,
	list vmLister,
	terminate vmTerminator,
) (orphans []string, err error) {
	owned := make(map[string]bool)
	for _, p := range ps.List() {
		if p.VMID != "" {
			owned[p.VMID] = true
		}
	}
	vms, listErr := list(ctx)
	for _, entry := range vms {
		vm := entry.VM
		if vm == nil || vm.ID == "" || owned[vm.ID] {
			continue
		}
		if !sandrpodVMName(vm.Name) {
			continue
		}
		orphans = append(orphans, vm.ID)
		if mode == sweepAlert {
			log.Printf("cloud sweeper: ALERT orphan VM %s (%s, provider %s) has no poder record", vm.ID, vm.Name, entry.Provider)
			continue
		}
		if err := terminate(ctx, entry.Provider, vm.ID); err != nil {
			log.Printf("cloud sweeper: orphan VM %s terminate failed, retrying next pass: %v", vm.ID, err)
			continue
		}
		log.Printf("cloud sweeper: released orphan VM %s (%s, provider %s)", vm.ID, vm.Name, entry.Provider)
		notifyEvent("vm.orphan_released", map[string]any{"vm_id": vm.ID, "provider": entry.Provider, "name": vm.Name})
	}
	return orphans, listErr
}

// sweepIntervalFromEnv exposes the parsed interval for tests.
func sweepIntervalFromEnv() time.Duration {
	if v := os.Getenv("SANDRPOD_CLOUD_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

// kept for symmetry with the other env knobs; referenced by tests.
var _ = strconv.Itoa
