package mcpbridge

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Verifies the supervisor waits longer between successive restarts (i.e.
// backoff grows). We use a transport that immediately fails Ping so the
// supervisor keeps restarting; each restart should take at least the
// previous backoff doubled.
func TestSupervisor_BackoffGrowsAcrossConsecutiveDeaths(t *testing.T) {
	// Track when each new spawn happens.
	var spawnTimes []time.Time
	var spawnCount atomic.Int32

	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
		spawnTimes = append(spawnTimes, time.Now())
		spawnCount.Add(1)
		// Tools list succeeds so the child enters ready; then Ping
		// fails forever, triggering the death/restart loop.
		return &alwaysFailPing{fakeTransport: fakeTransport{tools: []mcp.Tool{mkTool("t", "")}}}, nil
	}
	t.Cleanup(func() { newRealChildTransport = prev })

	cfgPath := writeCfg(t, `{"mcpServers":{"flapping":{"command":"x","sandrpod":{"max_restart_per_min":10}}}}`)
	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		SupervisorInterval: 50 * time.Millisecond,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// Wait long enough for 3 restart cycles: backoff sums = 1+2+4 = 7s.
	// Give it slack for ping interval and scheduling.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCount.Load() >= 4 { // initial + 3 restarts
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if spawnCount.Load() < 4 {
		t.Fatalf("expected at least 4 spawns (initial + 3 restarts), got %d", spawnCount.Load())
	}

	// Verify the gap between restart N and N+1 grew over time.
	// Skip index 0 (initial spawn). Compare gap[1->2] vs gap[2->3].
	gap12 := spawnTimes[2].Sub(spawnTimes[1])
	gap23 := spawnTimes[3].Sub(spawnTimes[2])
	if gap23 <= gap12 {
		t.Errorf("backoff didn't grow: gap1->2=%s, gap2->3=%s", gap12, gap23)
	}
	// And the first restart should NOT have been instant (>= 1s backoff).
	gap01 := spawnTimes[1].Sub(spawnTimes[0])
	if gap01 < 900*time.Millisecond {
		t.Errorf("first restart fired too fast: %s (expected >= 1s backoff)", gap01)
	}
}

type alwaysFailPing struct {
	fakeTransport
}

func (*alwaysFailPing) Ping(context.Context) error { return errFakeClosed }
