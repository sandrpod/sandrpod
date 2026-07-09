package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeOAuthMCPServer is one httptest server playing both roles: the OAuth 2.1
// authorization server (RFC8414 metadata + RFC7591 DCR + token endpoint) and
// an MCP Streamable-HTTP server that requires a Bearer token.
func fakeOAuthMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	const accessToken = "test-access-token"
	mux := http.NewServeMux()
	var base string

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           base,
			"authorization_endpoint":           base + "/authorize",
			"token_endpoint":                   base + "/token",
			"registration_endpoint":            base + "/register",
			"response_types_supported":         []string{"code"},
			"grant_types_supported":            []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"client_id": "test-client-id"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "fake-code" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{
			"access_token": accessToken, "token_type": "Bearer",
			"refresh_token": "test-refresh", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+accessToken {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-oauth-mcp", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "echo back",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		default: // ping etc.
			result = map[string]any{}
		}
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	})

	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// TestOAuth_EndToEnd drives the full ceremony against a fake provider:
// auth=oauth entry → child parks in waiting_auth with an authorization URL →
// (simulated) browser hits the loopback callback with the code → token
// exchanged + persisted → child restarts and comes up ready with tools.
func TestOAuth_EndToEnd(t *testing.T) {
	provider := fakeOAuthMCPServer(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	cfg := `{"mcpServers":{"fake":{"url":"` + provider.URL + `/mcp","auth":"oauth"}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	authURLs := make(chan string, 1)
	mgr := NewManager(ManagerOptions{
		ConfigPath: cfgPath,
		Logger:     quietLogger(),
		OAuth: &OAuthOptions{
			TokenDir:                filepath.Join(dir, "oauth"),
			CallbackAddr:            "127.0.0.1:0",
			OnAuthorizationRequired: func(_, u string) { authURLs <- u },
		},
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// 1) The child must park in waiting_auth and hand out an authorization URL.
	var authURL string
	select {
	case authURL = <-authURLs:
	case <-time.After(5 * time.Second):
		t.Fatal("OnAuthorizationRequired never fired")
	}
	snap := mgr.Snapshot()
	if len(snap) != 1 || snap[0].State != string(StateWaitingAuth) {
		t.Fatalf("expected waiting_auth, got %+v", snap)
	}

	// 2) Simulate the human: the AS would redirect the browser to our loopback
	// callback with a code. Extract redirect_uri+state from the auth URL.
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	redirect, state := q.Get("redirect_uri"), q.Get("state")
	if redirect == "" || state == "" {
		t.Fatalf("auth URL missing redirect_uri/state: %s", authURL)
	}
	resp, err := http.Get(redirect + "?code=fake-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback returned %d", resp.StatusCode)
	}

	// 3) Token stored + child restarted → ready with the provider's tools.
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap = mgr.Snapshot()
		if len(snap) == 1 && snap[0].State == string(StateReady) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never became ready: %+v", snap)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if snap[0].ToolCount != 1 {
		t.Fatalf("tool count = %d, want 1", snap[0].ToolCount)
	}
	// Token persisted → a later full restart would skip the browser entirely.
	if _, err := os.Stat(filepath.Join(dir, "oauth", "fake.json")); err != nil {
		t.Fatalf("token file not persisted: %v", err)
	}
}
