// Copyright 2026 SandrPod Contributors

package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
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

// captureSink records events for assertions.
type captureSink struct {
	mu     sync.Mutex
	events []struct {
		Name string
		Data map[string]any
	}
}

func (c *captureSink) Emit(event string, data map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, struct {
		Name string
		Data map[string]any
	}{event, data})
}

func (c *captureSink) find(name string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Name == name {
			return e.Data
		}
	}
	return nil
}

// Deleting a sandbox removes the record, so the event has to be built from what
// was captured first. It is the event that closes a billing period — losing
// owner and resources here means the last minute of usage is unattributable.
func TestSandboxDeleted_CarriesBillingFields(t *testing.T) {
	sink := &captureSink{}
	old := events
	events = sink
	t.Cleanup(func() { events = old })

	stores := store.NewMemoryStores()
	if err := stores.Sandboxes.Add(&podpkg.SandboxInfo{
		Name: "metered", Owner: "alice", State: podpkg.StateRunning,
		ProviderType: "local", CPUCores: 2, MemoryMB: 2048, TTLSeconds: 600,
		PoderID: "poder-1", ProxyURL: "tunnel://poder-1",
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{}, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/metered", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}

	ev := sink.find("sandbox.deleted")
	if ev == nil {
		t.Fatal("no sandbox.deleted event")
	}
	if ev["owner"] != "alice" || ev["cpu_cores"] != 2.0 || ev["memory_mb"] != int64(2048) {
		t.Fatalf("delete event lost the billing fields: %v", ev)
	}
}
