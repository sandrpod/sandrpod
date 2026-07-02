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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldReapSandbox(&c.sb, now, ttl); got != c.want {
				t.Errorf("shouldReapSandbox = %v, want %v", got, c.want)
			}
		})
	}
}
