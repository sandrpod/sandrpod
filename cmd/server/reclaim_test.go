package main

import (
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

func TestShouldReapSandbox(t *testing.T) {
	now := time.Now()
	ttl := time.Hour
	old := now.Add(-2 * time.Hour)
	fresh := now.Add(-time.Minute)

	cases := []struct {
		name string
		sb   podpkg.SandboxInfo
		want bool
	}{
		{"idle running is reaped", podpkg.SandboxInfo{State: podpkg.StateRunning, LastActivity: old}, true},
		{"idle stopped is reaped", podpkg.SandboxInfo{State: podpkg.StateStopped, LastActivity: old}, true},
		{"idle error is reaped", podpkg.SandboxInfo{State: podpkg.StateError, LastActivity: old}, true},
		{"active running survives", podpkg.SandboxInfo{State: podpkg.StateRunning, LastActivity: fresh}, false},
		{"pending is never reaped", podpkg.SandboxInfo{State: podpkg.StatePending, LastActivity: old}, false},
		{"direct agent is never reaped", podpkg.SandboxInfo{State: podpkg.StateRunning, ProxyURL: "direct://x", LastActivity: old}, false},
		{"zero activity falls back to created", podpkg.SandboxInfo{State: podpkg.StateRunning, CreatedAt: old}, true},
		{"zero activity + fresh created survives", podpkg.SandboxInfo{State: podpkg.StateRunning, CreatedAt: fresh}, false},
		{"both timestamps zero is skipped", podpkg.SandboxInfo{State: podpkg.StateRunning}, false},
		// A per-sandbox TTLSeconds (e.g. an E2B Sandbox.create(timeout=…))
		// overrides the global default in both directions.
		{"short per-sandbox TTL reaps a would-be-fresh sandbox", podpkg.SandboxInfo{State: podpkg.StateRunning, LastActivity: fresh, TTLSeconds: 30}, true},
		{"long per-sandbox TTL keeps an otherwise-idle sandbox", podpkg.SandboxInfo{State: podpkg.StateRunning, LastActivity: old, TTLSeconds: 3 * 3600}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldReapSandbox(&c.sb, now, ttl); got != c.want {
				t.Errorf("shouldReapSandbox = %v, want %v", got, c.want)
			}
		})
	}
}

func TestEffectiveSandboxTTL(t *testing.T) {
	def := time.Hour
	if got := effectiveSandboxTTL(&podpkg.SandboxInfo{}, def); got != def {
		t.Errorf("no per-sandbox TTL: got %v, want default %v", got, def)
	}
	if got := effectiveSandboxTTL(&podpkg.SandboxInfo{TTLSeconds: 300}, def); got != 5*time.Minute {
		t.Errorf("per-sandbox TTL: got %v, want 5m", got)
	}
}
