// Copyright 2026 SandrPod Contributors
package server

import (
	"strings"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

func TestCheckGPUQuota(t *testing.T) {
	q := gpuQuota{MaxPerOwner: 1, MaxTTL: 2 * time.Hour}
	live := []*podpkg.SandboxInfo{
		{Name: "a", Owner: "alice", GPU: 1, State: podpkg.StateRunning},
		{Name: "b", Owner: "alice", GPU: 0, State: podpkg.StateRunning}, // CPU: not counted
		{Name: "c", Owner: "bob", GPU: 1, State: podpkg.StateError},     // bob: dead record
		{Name: "d", Owner: "alice", GPU: 1, State: podpkg.StateError},   // dead record: not a slot
	}
	if msg := checkGPUQuota(q, &podpkg.CreateSandboxRequest{GPU: 1}, "alice", live); msg == "" {
		t.Fatal("alice already runs a GPU sandbox; the second must be refused")
	} else if !strings.Contains(msg, "capped at 1") {
		t.Fatalf("refusal should explain the cap, got %q", msg)
	}
	if msg := checkGPUQuota(q, &podpkg.CreateSandboxRequest{GPU: 1}, "bob", live); msg != "" {
		t.Fatalf("bob holds one GPU sandbox and the cap is 1, but an ERROR record must not count: %q", msg)
	}
	if msg := checkGPUQuota(q, &podpkg.CreateSandboxRequest{GPU: 2}, "carol", live); msg != "" {
		t.Fatalf("carol holds nothing: %q", msg)
	}
	// A CPU sandbox is never subject to the GPU cap.
	if msg := checkGPUQuota(q, &podpkg.CreateSandboxRequest{GPU: 0}, "alice", live); msg != "" {
		t.Fatalf("a CPU create must not be refused by the GPU cap: %q", msg)
	}
}

func TestClampGPUTTL(t *testing.T) {
	q := gpuQuota{MaxPerOwner: 1, MaxTTL: 2 * time.Hour}
	req := &podpkg.CreateSandboxRequest{GPU: 1, TTLSeconds: int64(24 * time.Hour / time.Second)}
	clamped, _, capped := clampGPUTTL(q, req)
	if !clamped || capped != 2*time.Hour {
		t.Fatalf("a 24h GPU TTL must clamp to 2h (clamped=%v capped=%v)", clamped, capped)
	}
	if req.TTLSeconds != int64(2*time.Hour/time.Second) {
		t.Fatalf("TTLSeconds = %d, want %d", req.TTLSeconds, int64(2*time.Hour/time.Second))
	}
	// A request already inside the window is untouched, and a CPU sandbox is
	// never clamped by the GPU policy.
	short := &podpkg.CreateSandboxRequest{GPU: 1, TTLSeconds: 1800}
	if clamped, _, _ := clampGPUTTL(q, short); clamped {
		t.Fatal("a 30m GPU TTL must not be clamped")
	}
	cpu := &podpkg.CreateSandboxRequest{TTLSeconds: 86400}
	if clamped, _, _ := clampGPUTTL(q, cpu); clamped {
		t.Fatal("a CPU sandbox must not be clamped by the GPU TTL policy")
	}
}

func TestGPUQuotaFromEnvDefaults(t *testing.T) {
	for _, k := range []string{"SANDRPOD_GPU_MAX_PER_OWNER", "SANDRPOD_GPU_MAX_TTL"} {
		t.Setenv(k, "")
	}
	q := gpuQuotaFromEnv()
	if q.MaxPerOwner != defaultGPUPerOwner || q.MaxTTL != defaultGPUMaxTTL {
		t.Fatalf("defaults = %+v, want %d / %v", q, defaultGPUPerOwner, defaultGPUMaxTTL)
	}
}
