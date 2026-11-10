// Copyright 2026 SandrPod Contributors
package server

import (
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// A STOPPED sandbox keeps its data until the retention window passes, then it is
// deleted (record + volume). Getting this backwards deletes user data on the
// first idle tick, so the边界 is worth a test.
func TestStoppedTooLong(t *testing.T) {
	now := time.Now()
	retain := 24 * time.Hour
	cases := []struct {
		name string
		sb   podpkg.SandboxInfo
		want bool
	}{
		{"fresh", podpkg.SandboxInfo{State: podpkg.StateStopped, LastActivity: now.Add(-time.Hour)}, false},
		{"expired", podpkg.SandboxInfo{State: podpkg.StateStopped, LastActivity: now.Add(-25 * time.Hour)}, true},
		{"no activity falls back to created", podpkg.SandboxInfo{State: podpkg.StateStopped, CreatedAt: now.Add(-25 * time.Hour)}, true},
		{"retention disabled keeps forever", podpkg.SandboxInfo{State: podpkg.StateStopped, LastActivity: now.Add(-1000 * time.Hour)}, false},
	}
	for _, c := range cases {
		got := stoppedTooLong(&c.sb, now, retain)
		if c.name == "retention disabled keeps forever" {
			got = stoppedTooLong(&c.sb, now, 0)
		}
		if got != c.want {
			t.Errorf("%s: stoppedTooLong = %v, want %v", c.name, got, c.want)
		}
	}
}
