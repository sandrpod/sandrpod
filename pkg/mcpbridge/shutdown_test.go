package mcpbridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// slowTransport blocks each CallTool until the test releases it. Lets us
// verify Shutdown waits for in-flight calls.
type slowTransport struct {
	fakeTransport
	release   chan struct{}
	started   chan struct{}
	callCount int
	mu        sync.Mutex
}

func (s *slowTransport) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	s.callCount++
	s.mu.Unlock()
	close(s.started)
	select {
	case <-s.release:
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "done"}}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestShutdown_WaitsForInFlight(t *testing.T) {
	s := &slowTransport{
		fakeTransport: fakeTransport{tools: []mcp.Tool{mkTool("slow", "")}},
		release:       make(chan struct{}),
		started:       make(chan struct{}),
	}
	withFakeTransport(t, map[string]*fakeTransport{"x": &s.fakeTransport})
	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) { return s, nil }
	t.Cleanup(func() { newRealChildTransport = prev })

	cfgPath := writeCfg(t, `{"mcpServers":{"foo":{"command":"x"}}}`)
	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		SupervisorInterval: time.Hour,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Fire a call in a goroutine; it will block in slowTransport.CallTool.
	callDone := make(chan error, 1)
	go func() {
		_, err := m.Dispatch(context.Background(), "foo__slow", nil)
		callDone <- err
	}()

	// Wait for the call to actually enter the transport.
	<-s.started

	// Shutdown with a generous drain timeout; release the call partway
	// through to simulate it finishing during drain.
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- m.Shutdown(context.Background(), 2*time.Second)
	}()

	// Give the shutdown goroutine a moment to start waiting.
	time.Sleep(100 * time.Millisecond)

	// Release the in-flight call.
	close(s.release)

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown didn't return after the in-flight call completed")
	}

	if err := <-callDone; err != nil {
		t.Errorf("in-flight call failed: %v", err)
	}
}

func TestShutdown_ReportsTimeoutOnSlowChild(t *testing.T) {
	s := &slowTransport{
		fakeTransport: fakeTransport{tools: []mcp.Tool{mkTool("slow", "")}},
		release:       make(chan struct{}), // never closed → call never completes
		started:       make(chan struct{}),
	}
	prev := newRealChildTransport
	newRealChildTransport = func(cfg ServerConfig) (childTransport, error) { return s, nil }
	t.Cleanup(func() { newRealChildTransport = prev })

	cfgPath := writeCfg(t, `{"mcpServers":{"foo":{"command":"x"}}}`)
	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		SupervisorInterval: time.Hour,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Fire a call in background.
	go func() { _, _ = m.Dispatch(context.Background(), "foo__slow", nil) }()
	<-s.started

	// Drain timeout is short — should hit it.
	err := m.Shutdown(context.Background(), 200*time.Millisecond)
	if err == nil {
		t.Errorf("expected drain timeout error")
	}

	// Children must still be torn down even though drain failed.
	if got := len(m.Snapshot()); got != 0 {
		t.Errorf("expected children cleared after Shutdown, got %d", got)
	}
}
