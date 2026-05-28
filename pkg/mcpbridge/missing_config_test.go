package mcpbridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestStart_MissingConfigStartsEmpty asserts the bridge comes up with
// zero children when mcp.json is absent at start time (no error
// returned). Documents the deliberate UX: "install the agent first,
// drop in mcp.json later" must just work.
func TestStart_MissingConfigStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json") // file does NOT exist

	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		SupervisorInterval: time.Hour,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start with missing config returned error: %v", err)
	}
	defer m.Stop(context.Background())

	if got := len(m.Snapshot()); got != 0 {
		t.Errorf("expected 0 children when no config, got %d", got)
	}
	if got := len(m.AggregatedTools()); got != 0 {
		t.Errorf("expected 0 tools, got %d", got)
	}
}

// TestStart_ParentDirCreated asserts the watcher creates the conventional
// parent dir if it doesn't exist — otherwise fsnotify.Add fails and
// hot-reload is silently degraded.
func TestStart_ParentDirCreated(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "deep", "nested", "sandrpod") // doesn't exist yet
	cfgPath := filepath.Join(nested, "mcp.json")

	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		HotReload:          true,
		SupervisorInterval: time.Hour,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	if fi, err := os.Stat(nested); err != nil || !fi.IsDir() {
		t.Errorf("expected parent dir to be created at %s, err=%v", nested, err)
	}
}

// TestHotReload_FileCreateAfterStart covers the headline UX: agent up,
// no mcp.json; user `cp` a config in; new servers appear within the
// debounce window without restarting the agent.
func TestHotReload_FileCreateAfterStart(t *testing.T) {
	fakes := map[string]*fakeTransport{
		"a-bin": {tools: []mcp.Tool{mkTool("a1", "")}},
	}
	withFakeTransport(t, fakes)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		HotReload:          true,
		SupervisorInterval: time.Hour,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if len(m.AggregatedTools()) != 0 {
		t.Fatalf("expected 0 tools at start")
	}

	// Drop in the config (atomic create via tmp + rename, the editor
	// pattern most likely to break naive watchers).
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"mcpServers":{"a":{"command":"a-bin"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.AggregatedTools()) == 1 {
			return
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatalf("hot-reload didn't pick up newly-created config, tools=%d", len(m.AggregatedTools()))
}

// TestHotReload_FileDeleteTearsDown covers the reverse: a config that
// existed at start is later removed → all children stop, /mcp/manifest
// reports zero servers. Important so `rm mcp.json` is a clean
// "disable everything" lever.
func TestHotReload_FileDeleteTearsDown(t *testing.T) {
	fakes := map[string]*fakeTransport{
		"a-bin": {tools: []mcp.Tool{mkTool("a1", "")}},
	}
	withFakeTransport(t, fakes)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(cfgPath, []byte(`{"mcpServers":{"a":{"command":"a-bin"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		HotReload:          true,
		SupervisorInterval: time.Hour,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if len(m.AggregatedTools()) != 1 {
		t.Fatalf("expected 1 tool at start, got %d", len(m.AggregatedTools()))
	}

	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.Snapshot()) == 0 {
			return
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatalf("delete didn't tear down children, snapshot=%d", len(m.Snapshot()))
}

// Parse errors should NOT be swallowed — they signal a user mistake we
// want to surface, not silently come up empty.
func TestStart_BadJSONStillErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(cfgPath, []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(ManagerOptions{ConfigPath: cfgPath, SupervisorInterval: time.Hour})
	if err := m.Start(context.Background()); err == nil {
		t.Errorf("expected parse error to surface, got nil")
	}
}
