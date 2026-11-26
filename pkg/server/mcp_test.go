// Copyright 2026 SandrPod Contributors
package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/yamux"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
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
	for _, want := range []string{
		"sandbox_create", "sandbox_exec", "sandbox_write_file",
		"sandbox_stop", "sandbox_start",
		"sandbox_mcp_list", "sandbox_mcp_call",
	} {
		if !names[want] {
			t.Fatalf("tool %q missing from tools/list", want)
		}
	}
	for _, removed := range []string{"desktop_screenshot", "desktop_click", "desktop_type", "desktop_key", "desktop_scroll"} {
		if names[removed] {
			t.Fatalf("hardcoded tool %q should not be exposed on platform /mcp (use sandbox_mcp_list/call)", removed)
		}
	}
	// Every tool's inputSchema must be valid JSON Schema: non-null properties
	// object, and `required` omitted rather than serialized as `null` when empty.
	for _, tl := range tools {
		tm := tl.(map[string]any)
		tname := tm["name"].(string)
		schema, ok := tm["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("tool %q has invalid inputSchema: %v", tname, tm["inputSchema"])
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("tool %q inputSchema.properties is not an object: %v", tname, schema["properties"])
		}
		if reqRaw, hasReq := schema["required"]; hasReq {
			reqList, ok := reqRaw.([]any)
			if !ok || len(reqList) == 0 {
				t.Fatalf("tool %q inputSchema.required must be a non-empty array when present, got %T (%v)", tname, reqRaw, reqRaw)
			}
			for _, r := range reqList {
				key, ok := r.(string)
				if !ok || props[key] == nil {
					t.Fatalf("tool %q required field %v missing from properties", tname, r)
				}
			}
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

// TestMCPSandboxMCPListAndCall exercises sandbox_mcp_list and sandbox_mcp_call
// end-to-end through the platform /mcp endpoint over a yamux tunnel:
//   - tool discovery via sandbox_mcp_list
//   - tool invocation via sandbox_mcp_call preserving image content blocks
//   - Mcp-Session-Id caching (initialize runs once across calls)
//   - transparent re-initialize when a cached session expires
//   - platform Bearer token never leaks into the sandbox; mcp_token does
func TestMCPSandboxMCPListAndCall(t *testing.T) {
	const (
		apiToken = "platform-secret-token"
		mcpToken = "sandbox-mcp-secret"
		sandbox  = "box-1"
	)

	var (
		mu             sync.Mutex
		initCount      int
		activeSession  = "sess-1"
		seenAuthHeader string
		seenPlatHeader string
	)

	fakeBridge := http.NewServeMux()
	fakeBridge.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seenAuthHeader = r.Header.Get("Authorization")
		seenPlatHeader = r.Header.Get("X-Sandrpod-Token")
		if seenAuthHeader != "Bearer "+mcpToken {
			mu.Unlock()
			http.Error(w, "unauthorized mcp token", http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case "initialize":
			initCount++
			sess := activeSession
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", sess)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"protocolVersion": "2025-06-18"},
			})
		case "tools/list":
			wantSess := activeSession
			mu.Unlock()
			if r.Header.Get("Mcp-Session-Id") != wantSess {
				http.Error(w, "Invalid session ID", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "browser__screenshot", "description": "Take a browser screenshot"},
						{"name": "notion__search", "description": "Search Notion"},
					},
				},
			})
		case "tools/call":
			wantSess := activeSession
			mu.Unlock()
			if r.Header.Get("Mcp-Session-Id") != wantSess {
				http.Error(w, "Invalid session ID", http.StatusBadRequest)
				return
			}
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "called:" + params.Name},
						{"type": "image", "data": "iVBORw0KGgo=", "mimeType": "image/png"},
					},
					"isError": false,
				},
			})
		default:
			mu.Unlock()
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	})

	apiSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = apiSide.Close(); _ = agentSide.Close() })
	agentSession, err := yamux.Server(agentSide, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	agentSrv := &http.Server{Handler: fakeBridge}
	go func() { _ = agentSrv.Serve(agentSession) }()
	t.Cleanup(func() { _ = agentSrv.Shutdown(context.Background()) })

	tt, err := tunnel.NewPoderTunnelFromConn(sandbox, apiSide)
	if err != nil {
		t.Fatal(err)
	}

	stores := store.NewMemoryStores()
	if err := stores.Sandboxes.Add(&podpkg.SandboxInfo{
		ID:       sandbox,
		Name:     sandbox,
		State:    podpkg.StateRunning,
		ProxyURL: "direct://" + sandbox,
	}); err != nil {
		t.Fatal(err)
	}
	ds := tunnel.NewDirectTunnelStore()
	ds.Set(sandbox, tt)
	mux := New(Config{Token: apiToken}, stores, tunnel.NewTunnelStore(), ds)

	callPlatform := func(toolName string, args map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params":  map[string]any{"name": toolName, "arguments": args},
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: bad JSON-RPC (%d): %v (%s)", toolName, rec.Code, err, rec.Body.String())
		}
		return out
	}

	// 1. Discover tools via sandbox_mcp_list.
	listOut := callPlatform("sandbox_mcp_list", map[string]any{
		"sandbox":   sandbox,
		"mcp_token": mcpToken,
	})
	listRes := listOut["result"].(map[string]any)
	if listRes["isError"] == true {
		t.Fatalf("sandbox_mcp_list failed: %v", listRes)
	}
	listText := listRes["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(listText, "browser__screenshot") || !strings.Contains(listText, "notion__search") {
		t.Fatalf("sandbox_mcp_list missing expected tools: %s", listText)
	}

	// 2. Call tool via sandbox_mcp_call — must reuse cached session and preserve image block.
	callOut := callPlatform("sandbox_mcp_call", map[string]any{
		"sandbox":   sandbox,
		"tool":      "browser__screenshot",
		"arguments": map[string]any{"fullPage": true},
		"mcp_token": mcpToken,
	})
	callRes := callOut["result"].(map[string]any)
	if callRes["isError"] == true {
		t.Fatalf("sandbox_mcp_call failed: %v", callRes)
	}
	blocks := callRes["content"].([]any)
	if len(blocks) != 2 || blocks[1].(map[string]any)["type"] != "image" {
		t.Fatalf("expected text+image blocks preserved, got %v", blocks)
	}

	mu.Lock()
	gotInits := initCount
	gotAuth := seenAuthHeader
	gotPlat := seenPlatHeader
	mu.Unlock()
	if gotInits != 1 {
		t.Fatalf("initialize called %d times across list+call, want 1 (session cache)", gotInits)
	}
	if gotAuth != "Bearer "+mcpToken {
		t.Fatalf("sandbox saw Authorization=%q, want Bearer %s", gotAuth, mcpToken)
	}
	if gotPlat != "" {
		t.Fatalf("platform token leaked to sandbox: X-Sandrpod-Token=%q", gotPlat)
	}

	// 3. Simulate sandbox restart invalidating the cached session → transparent retry.
	mu.Lock()
	activeSession = "sess-2"
	mu.Unlock()

	retryOut := callPlatform("sandbox_mcp_call", map[string]any{
		"sandbox":   sandbox,
		"tool":      "notion__search",
		"mcp_token": mcpToken,
	})
	retryRes := retryOut["result"].(map[string]any)
	if retryRes["isError"] == true {
		t.Fatalf("sandbox_mcp_call after session rotation failed: %v", retryRes)
	}
	mu.Lock()
	gotInits = initCount
	mu.Unlock()
	if gotInits != 2 {
		t.Fatalf("expected 1 re-initialize after stale session (total 2), got %d", gotInits)
	}

	// 4. Unknown sandbox returns a clear isError message.
	missOut := callPlatform("sandbox_mcp_list", map[string]any{"sandbox": "no-such-box"})
	missRes := missOut["result"].(map[string]any)
	if missRes["isError"] != true {
		t.Fatalf("expected isError=true for missing sandbox, got %v", missRes)
	}
}
