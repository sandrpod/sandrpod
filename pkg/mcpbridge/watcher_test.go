package mcpbridge

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestHotReload_FsnotifyTriggersReload(t *testing.T) {
	fakes := map[string]*fakeTransport{
		"a-bin": {tools: []mcp.Tool{mkTool("a1", "")}},
		"b-bin": {tools: []mcp.Tool{mkTool("b1", "")}},
	}
	withFakeTransport(t, fakes)
	cfgPath := writeCfg(t, `{"mcpServers":{"a":{"command":"a-bin"}}}`)

	m := NewManager(ManagerOptions{
		ConfigPath:         cfgPath,
		HotReload:          true,
		SupervisorInterval: time.Hour, // disable the ping ticker in this test
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if len(m.AggregatedTools()) != 1 {
		t.Fatalf("expected 1 tool initially")
	}

	// Rewrite the config with a second server. Atomic-replace is the
	// realistic editor-save path, so do that.
	tmp := cfgPath + ".new"
	if err := os.WriteFile(tmp, []byte(`{"mcpServers":{"a":{"command":"a-bin"},"b":{"command":"b-bin"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.AggregatedTools()) == 2 {
			return
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatalf("hot-reload didn't pick up new server within deadline, tools=%d", len(m.AggregatedTools()))
}
