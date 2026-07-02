package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// TestMCPRouteForwardsBearerToAgent is the regression test for the auth
// header conflict fix (docs/MCP_AUTH_HEADER_CONFLICT_FIX.md):
//
//  1. API Server is configured with cfg.Token = api-token-abc.
//  2. A fake agent is wired in over an in-memory yamux session — it
//     records the Authorization header of every request it sees.
//  3. A client calls /api/v1/sandboxes/test-sandbox/mcp/manifest with
//     BOTH headers: X-Sandrpod-Token (the API auth) and Authorization
//     (the MCP Bearer meant for the agent).
//  4. We assert:
//     - The API Server lets the request through (no 401).
//     - The agent receives Authorization unchanged — i.e. the MCP
//     Bearer reached it, not cfg.Token.
//
// Before the fix, step 4b would fail: cfg.Token in Authorization was
// the only thing that satisfied the API Server, so clients couldn't
// also pass a different value for the agent to validate.
func TestMCPRouteForwardsBearerToAgent(t *testing.T) {
	const (
		apiToken  = "api-token-abc"
		mcpBearer = "Bearer mcp-token-xyz"
		sandbox   = "test-sandbox"
	)

	// --- 1) Fake agent: records Authorization, returns 200. ----------------
	var (
		agentMu         sync.Mutex
		seenAuth        string
		seenSandrpodTok string
	)
	fakeAgent := http.NewServeMux()
	fakeAgent.HandleFunc("/mcp/manifest", func(w http.ResponseWriter, r *http.Request) {
		agentMu.Lock()
		seenAuth = r.Header.Get("Authorization")
		seenSandrpodTok = r.Header.Get("X-Sandrpod-Token")
		agentMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"servers":[],"total_tools":0}`))
	})
	agentSrv := &http.Server{Handler: fakeAgent}

	// --- 2) Yamux pipe between API Server (client) and fake agent (server) ---
	apiSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = apiSide.Close(); _ = agentSide.Close() })

	// Agent end serves HTTP over yamux (mirroring the real agent).
	agentSession, err := yamux.Server(agentSide, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = agentSrv.Serve(agentSession) }()
	t.Cleanup(func() { _ = agentSrv.Shutdown(context.Background()) })

	// API Server end uses the existing tunnel constructor.
	tt, err := tunnel.NewPoderTunnelFromConn(sandbox, apiSide)
	if err != nil {
		t.Fatal(err)
	}

	// --- 3) Mux + stores wired to look like a direct-mode sandbox. ----------
	stores := store.NewMemoryStores()
	if err := stores.Sandboxes.Add(&podpkg.SandboxInfo{
		ID:       sandbox,
		Name:     sandbox,
		State:    podpkg.StateRunning,
		ProxyURL: "direct://" + sandbox,
	}); err != nil {
		t.Fatal(err)
	}
	ts := tunnel.NewTunnelStore()
	ds := tunnel.NewDirectTunnelStore()
	ds.Set(sandbox, tt)

	mux := buildMux(serverConfig{Token: apiToken}, stores, ts, ds)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// --- 4) The actual test: two different secrets in two different headers ---
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/sandboxes/"+sandbox+"/mcp/manifest", nil)
	req.Header.Set("X-Sandrpod-Token", apiToken)
	req.Header.Set("Authorization", mcpBearer)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (body: %s)", resp.StatusCode, body)
	}

	agentMu.Lock()
	defer agentMu.Unlock()
	if seenAuth != mcpBearer {
		t.Errorf("agent saw Authorization=%q, want %q\n  → BUG: API Server rewrote or consumed the MCP Bearer header", seenAuth, mcpBearer)
	}
	// X-Sandrpod-Token is also forwarded (we don't filter headers in
	// proxyHTTPStreaming). This is fine — the agent ignores headers it
	// doesn't recognize. But assert anyway so accidental filtering of
	// X-Sandrpod-Token (e.g. a future "strip auth headers" change)
	// shows up here.
	if seenSandrpodTok != apiToken {
		t.Errorf("agent saw X-Sandrpod-Token=%q, want %q", seenSandrpodTok, apiToken)
	}
}

// TestMCPRoute_WrongMCPBearer asserts the second tier of auth: even
// with a valid X-Sandrpod-Token, a wrong/missing MCP Bearer reaching
// the agent must produce an unauthorized response from the agent (the
// API Server isn't expected to validate it; that's the agent's job).
func TestMCPRoute_WrongMCPBearer(t *testing.T) {
	const (
		apiToken         = "api-token-abc"
		expectedMCPToken = "right-mcp"
		sandbox          = "test-sandbox"
	)

	// Fake agent enforces its own Bearer.
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("/mcp/manifest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectedMCPToken {
			http.Error(w, "unauthorized (agent side)", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	apiSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = apiSide.Close(); _ = agentSide.Close() })

	agentSession, err := yamux.Server(agentSide, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = (&http.Server{Handler: agentMux}).Serve(agentSession) }()

	tt, err := tunnel.NewPoderTunnelFromConn(sandbox, apiSide)
	if err != nil {
		t.Fatal(err)
	}

	stores := store.NewMemoryStores()
	_ = stores.Sandboxes.Add(&podpkg.SandboxInfo{
		ID: sandbox, Name: sandbox, State: podpkg.StateRunning, ProxyURL: "direct://" + sandbox,
	})
	ts := tunnel.NewTunnelStore()
	ds := tunnel.NewDirectTunnelStore()
	ds.Set(sandbox, tt)
	mux := buildMux(serverConfig{Token: apiToken}, stores, ts, ds)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Pass valid API token but wrong MCP Bearer.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/sandboxes/"+sandbox+"/mcp/manifest", nil)
	req.Header.Set("X-Sandrpod-Token", apiToken)
	req.Header.Set("Authorization", "Bearer WRONG")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// API Server let it through; the agent rejected it. Either status
	// (401 from agent) reaching the client is fine — that's exactly
	// the "defense in depth" we want.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 from agent layer, got %d", resp.StatusCode)
	}
}
