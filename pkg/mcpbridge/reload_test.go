package mcpbridge

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestReload_AddRemoveDiff(t *testing.T) {
	fakes := map[string]*fakeTransport{
		"a-bin": {tools: []mcp.Tool{mkTool("a1", "")}},
		"b-bin": {tools: []mcp.Tool{mkTool("b1", "")}},
		"c-bin": {tools: []mcp.Tool{mkTool("c1", "")}},
	}
	withFakeTransport(t, fakes)
	cfgPath := writeCfg(t, `{"mcpServers":{
	  "a": {"command":"a-bin"},
	  "b": {"command":"b-bin"}
	}}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if got := len(m.AggregatedTools()); got != 2 {
		t.Fatalf("initial tool count = %d", got)
	}

	// Remove "a", add "c", keep "b" unchanged.
	if err := os.WriteFile(cfgPath, []byte(`{"mcpServers":{
	  "b": {"command":"b-bin"},
	  "c": {"command":"c-bin"}
	}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, tl := range m.AggregatedTools() {
		names[tl.Name] = true
	}
	if names["a__a1"] {
		t.Errorf("removed server's tool still present")
	}
	if !names["b__b1"] || !names["c__c1"] {
		t.Errorf("expected b__b1 and c__c1, got %v", names)
	}

	// Verify "b" was not restarted: its fake should still be the same
	// instance, and the closed flag should be false.
	if fakes["b-bin"].closed {
		t.Errorf("unchanged server 'b' was needlessly restarted")
	}
}

func TestReload_ConfigChangeRestartsServer(t *testing.T) {
	f := &fakeTransport{tools: []mcp.Tool{mkTool("t", "")}}
	withFakeTransport(t, map[string]*fakeTransport{"x": f})
	cfgPath := writeCfg(t, `{"mcpServers":{"a":{"command":"x"}}}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// Change env (would change hash) — should trigger restart.
	if err := os.WriteFile(cfgPath, []byte(`{"mcpServers":{"a":{"command":"x","env":{"NEW":"1"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.closed {
		// Restart calls Close on the old transport; if hash check works,
		// the underlying fake should have been Closed.
		t.Errorf("expected old transport to be closed on config change")
	}
}

func TestSupervisor_RestartsOnPingFailure(t *testing.T) {
	// Once Ping is asked twice it starts failing.
	pingCount := int32(0)
	f := &flakyTransport{
		fakeTransport: fakeTransport{tools: []mcp.Tool{mkTool("t", "")}},
		failPingAfter: 1,
		counter:       &pingCount,
	}
	// The restart will spawn the SAME factory again, producing a fresh fake.
	freshFakes := []*flakyTransport{
		f,
		{fakeTransport: fakeTransport{tools: []mcp.Tool{mkTool("t", "")}}, failPingAfter: 999, counter: &pingCount},
	}
	var idx atomic.Int32
	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) {
		i := int(idx.Add(1)) - 1
		if i >= len(freshFakes) {
			return nil, errFakeClosed
		}
		return freshFakes[i], nil
	}
	t.Cleanup(func() { newRealChildTransport = prev })

	cfgPath := writeCfg(t, `{"mcpServers":{"foo":{"command":"x","sandrpod":{"restart_policy":"always","max_restart_per_min":3}}}}`)
	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		SupervisorInterval: 30 * time.Millisecond,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// Wait for at least one full restart cycle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if idx.Load() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if idx.Load() < 2 {
		t.Fatalf("supervisor did not restart child within deadline (idx=%d)", idx.Load())
	}
}

type flakyTransport struct {
	fakeTransport
	failPingAfter int32
	counter       *int32
}

func (f *flakyTransport) Ping(ctx context.Context) error {
	n := atomic.AddInt32(f.counter, 1)
	if n > f.failPingAfter {
		return errFakeClosed
	}
	return f.fakeTransport.Ping(ctx)
}
