package mcpbridge

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// A child whose first start fails (e.g. HTTP upstream not yet listening) must
// be retried by the supervisor and recover to ready once the transport
// succeeds — not stay failed until a config change.
func TestSupervisor_RecoversFailedChild(t *testing.T) {
	var attempts atomic.Int32
	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
		if attempts.Add(1) == 1 {
			return nil, errFakeClosed // first attempt: upstream down
		}
		return &fakeTransport{tools: []mcp.Tool{mkTool("t", "")}}, nil
	}
	t.Cleanup(func() { newRealChildTransport = prev })

	cfgPath := writeCfg(t, `{"mcpServers":{"late":{"url":"http://x/mcp","sandrpod":{"max_restart_per_min":10}}}}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath, SupervisorInterval: 50 * time.Millisecond})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// Initially the child failed its first start.
	if snap := m.Snapshot(); len(snap) != 1 || snap[0].State != string(StateFailed) {
		t.Fatalf("expected initial state failed, got %+v", snap)
	}

	// The supervisor should retry and bring it to ready.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := m.Snapshot()
		if len(snap) == 1 && snap[0].State == string(StateReady) {
			if attempts.Load() < 2 {
				t.Fatalf("recovered without a retry (attempts=%d)", attempts.Load())
			}
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("failed child was not recovered: %+v", m.Snapshot())
}

// restart_policy=never must keep a failed child down (no retry).
func TestSupervisor_RestartNeverStaysFailed(t *testing.T) {
	var attempts atomic.Int32
	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
		attempts.Add(1)
		return nil, errFakeClosed // always fail
	}
	t.Cleanup(func() { newRealChildTransport = prev })

	cfgPath := writeCfg(t, `{"mcpServers":{"down":{"url":"http://x/mcp","sandrpod":{"restart_policy":"never"}}}}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath, SupervisorInterval: 30 * time.Millisecond})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	time.Sleep(300 * time.Millisecond) // several sweep intervals
	if n := attempts.Load(); n != 1 {
		t.Fatalf("restart_policy=never should not retry; attempts=%d", n)
	}
}
