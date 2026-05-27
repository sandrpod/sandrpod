package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// mcpTokenMiddleware enforces a shared-secret check on every inbound MCP
// request when expectedToken is non-empty. Empty token means "no auth" —
// backward-compatible with deployments that trust the tunnel boundary
// (single-tenant API Server, LAN-only --mcp-only mode).
//
// Threat model addressed:
//
//	Without this gate, anyone who can reach the API Server's
//	/api/v1/sandboxes/{name}/mcp route can invoke any tool the bridge
//	exposes — including ones with privileged env-var credentials. If the
//	API Server itself is compromised, an attacker can replay or forge MCP
//	calls against every connected employee PC.
//
//	With this gate, the API Server proxies requests as opaque bytes (it
//	doesn't know the token), and the agent rejects anything missing the
//	correct Bearer. The token is shared out-of-band between the agent
//	operator and the MCP client (i.e. baked into the client's `mcp.json`
//	or LangChain config). A compromised API Server can still replay
//	captured requests but cannot forge new ones.
//
// The /admin/* endpoints are NOT covered by this middleware — they're
// served on a unix socket whose file permissions (0600) are the auth
// boundary.
//
// constant-time compare prevents the trivial timing oracle on the secret.
func mcpTokenMiddleware(expectedToken string, next http.Handler) http.Handler {
	if expectedToken == "" {
		return next
	}
	want := []byte(expectedToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := extractBearer(r.Header.Get("Authorization"))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			// Tell MCP clients exactly which auth scheme we expect so
			// they can populate the header on retry. Don't leak whether
			// the token was missing vs wrong.
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandrpod-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func extractBearer(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
