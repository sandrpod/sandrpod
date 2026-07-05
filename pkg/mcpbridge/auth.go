// Copyright 2024 SandrPod
// Shared MCP bridge auth: a constant-time Bearer guard for the /mcp surface,
// used by both cmd/agent and cmd/toolbox (previously duplicated in each).

package mcpbridge

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// TokenMiddleware guards the /mcp surface with a constant-time Bearer check.
//
// This is the personal/resource layer of the two-tier MCP auth model (see
// docs/MCP_AUTH.md). The API Server authenticates the caller at the platform
// layer (X-Sandrpod-Token) and forwards the request's Authorization header
// through the tunnel as opaque bytes; this middleware — running next to the
// bridge — is what actually validates that Bearer. Sharing the secret out of
// band with the MCP client means a compromised API Server can replay captured
// requests but cannot forge new ones.
//
// token == "" is a no-op (no MCP auth) — backward compatible with single-tenant
// deployments that trust the tunnel boundary. When set, every /mcp request must
// carry the matching Bearer, EXCEPT:
//
//   - /mcp/manifest is read-only metadata (server names, states, tool counts —
//     no credentials) and is exempt by default, so a caller already
//     authenticated at the platform layer can list tools without holding the
//     per-sandbox secret. Pass guardManifest=true to require the token there too.
//
// The response uses WWW-Authenticate so MCP clients learn the scheme, and does
// not distinguish missing vs wrong token.
func TokenMiddleware(token string, guardManifest bool, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !guardManifest && r.URL.Path == "/mcp/manifest" {
			next.ServeHTTP(w, r)
			return
		}
		got := extractBearer(r.Header.Get("Authorization"))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandrpod-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractBearer returns the token from an "Authorization: Bearer <token>"
// header, or "" when the header is absent or malformed.
func extractBearer(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
