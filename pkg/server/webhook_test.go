// Copyright 2026 SandrPod Contributors

package server

import (
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// A metering sink cannot reconstruct what a sandbox reserved from its name, so
// the lifecycle payload has to carry it. This pins the fields a billing
// integration reads: owner, resources, placement and creation time.
func TestSandboxEvent_CarriesBillingFields(t *testing.T) {
	sb := &podpkg.SandboxInfo{
		Name: "metered", Owner: "alice", ProviderType: "local", Region: "local",
		State: podpkg.StateRunning, CPUCores: 2, MemoryMB: 4096,
		TTLSeconds: 3600, PoderID: "poder-1",
		CreatedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	ev := sandboxEvent(sb, map[string]any{"extra": "kept"})

	want := map[string]any{
		"name": "metered", "owner": "alice", "provider": "local", "region": "local",
		"state": "RUNNING", "cpu_cores": 2.0, "memory_mb": int64(4096),
		"ttl_seconds": int64(3600), "poder_id": "poder-1",
		"created_at": "2026-10-26T12:00:00Z",
	}
	for k, v := range want {
		if ev[k] != v {
			t.Errorf("%s = %v, want %v", k, ev[k], v)
		}
	}
	if ev["extra"] != "kept" {
		t.Errorf("extra fields must survive, got %v", ev["extra"])
	}
}

// A sandbox created without explicit resources reports 0, not a missing key:
// a sink reading cpu_cores should see "the substrate default" rather than
// "unknown", and ttl_seconds is omitted rather than reported as zero.
func TestSandboxEvent_ZeroResourcesAndNoTTL(t *testing.T) {
	ev := sandboxEvent(&podpkg.SandboxInfo{Name: "plain", Owner: "bob"}, nil)
	if ev["cpu_cores"] != 0.0 || ev["memory_mb"] != int64(0) {
		t.Fatalf("zero resources should be reported as 0, got %v / %v", ev["cpu_cores"], ev["memory_mb"])
	}
	if _, ok := ev["ttl_seconds"]; ok {
		t.Error("ttl_seconds must be absent when no TTL was set")
	}
}
