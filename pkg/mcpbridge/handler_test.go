package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestHTTPHandler_Manifest(t *testing.T) {
	withFakeTransport(t, map[string]*fakeTransport{
		"gh": {tools: []mcp.Tool{mkTool("a", ""), mkTool("b", "")}},
	})
	cfgPath := writeCfg(t, `{"mcpServers":{"github":{"command":"gh"}}}`)
	m := NewManager(ManagerOptions{ConfigPath: cfgPath})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	srv := httptest.NewServer(NewHTTPHandler(m))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/mcp/manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var m2 Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m2); err != nil {
		t.Fatal(err)
	}
	if m2.TotalTools != 2 {
		t.Errorf("total tools = %d, want 2", m2.TotalTools)
	}
	if len(m2.Servers) != 1 || m2.Servers[0].Name != "github" {
		t.Errorf("unexpected servers slice: %+v", m2.Servers)
	}
}
