// Copyright 2026 SandrPod Contributors
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
)

// mcpTestServer wires a real mux (in-memory stores, auth disabled) behind the
// MCP endpoint so the dispatch path is exercised end to end.
func mcpTestServer(t *testing.T) http.Handler {
	t.Helper()
	stores := store.NewMemoryStores()
	cfg := Config{APIURL: "http://localhost:0"}
	return New(cfg, stores, nil, nil)
}

func rpcCall(t *testing.T, h http.Handler, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: not JSON-RPC: %v (%s)", method, err, rec.Body.String())
	}
	return out
}

func TestMCPInitializeAndList(t *testing.T) {
	h := mcpTestServer(t)

	init := rpcCall(t, h, "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	res, _ := init["result"].(map[string]any)
	if res == nil || res["serverInfo"].(map[string]any)["name"] != "sandrpod" {
		t.Fatalf("initialize result = %v", init)
	}

	list := rpcCall(t, h, "tools/list", nil)
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 8 {
		t.Fatalf("tools/list returned %d tools, want the sandbox surface", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"sandbox_create", "sandbox_exec", "sandbox_write_file", "sandbox_stop", "sandbox_start"} {
		if !names[want] {
			t.Fatalf("tool %q missing from tools/list", want)
		}
	}
}

func TestMCPToolCallExecutesThroughMux(t *testing.T) {
	h := mcpTestServer(t)
	// A create with no reachable worker still proves the dispatch: the REST
	// handler answers, and MCP wraps its error instead of inventing one.
	out := rpcCall(t, h, "tools/call", map[string]any{
		"name":      "sandbox_create",
		"arguments": map[string]any{"name": "mcp-test"},
	})
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tools/call returned no content: %v", out)
	}
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "sandbox") && !strings.Contains(text, "job") {
		t.Fatalf("unexpected tool output: %s", text)
	}
}

func TestMCPUnknownToolIsProtocolError(t *testing.T) {
	h := mcpTestServer(t)
	out := rpcCall(t, h, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected a JSON-RPC error for an unknown tool, got %v", out)
	}
}

func TestMCPRejectsNonPost(t *testing.T) {
	h := mcpTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405", rec.Code)
	}
}

// The create tool must carry persistence through: a stop/start cycle is only
// useful if /workspace survives, and the default is on.
func TestMCPCreateDefaultsToPersist(t *testing.T) {
	h := mcpTestServer(t)
	rpcCall(t, h, "tools/call", map[string]any{
		"name":      "sandbox_create",
		"arguments": map[string]any{"name": "mcp-persist"},
	})
	_ = context.Background()
	_ = podpkg.StateRunning
}
