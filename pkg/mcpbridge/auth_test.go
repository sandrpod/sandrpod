package mcpbridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func do(h http.Handler, path, bearer string) int {
	req := httptest.NewRequest("POST", path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestTokenMiddleware_NoToken_PassThrough(t *testing.T) {
	h := TokenMiddleware("", false, okHandler())
	if code := do(h, "/mcp", ""); code != http.StatusOK {
		t.Fatalf("empty token should be a no-op, got %d", code)
	}
}

func TestTokenMiddleware_GuardsMcp(t *testing.T) {
	h := TokenMiddleware("s3cret", false, okHandler())
	if code := do(h, "/mcp", ""); code != http.StatusUnauthorized {
		t.Errorf("/mcp no bearer: want 401, got %d", code)
	}
	if code := do(h, "/mcp", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("/mcp wrong bearer: want 401, got %d", code)
	}
	if code := do(h, "/mcp", "s3cret"); code != http.StatusOK {
		t.Errorf("/mcp right bearer: want 200, got %d", code)
	}
}

func TestTokenMiddleware_ManifestExemptByDefault(t *testing.T) {
	h := TokenMiddleware("s3cret", false, okHandler())
	// Read-only metadata: reachable without the personal token (platform auth
	// upstream is enough), while tool invocation on /mcp stays guarded.
	if code := do(h, "/mcp/manifest", ""); code != http.StatusOK {
		t.Errorf("/mcp/manifest no bearer (default exempt): want 200, got %d", code)
	}
}

func TestTokenMiddleware_ManifestGuardedWhenOptedIn(t *testing.T) {
	h := TokenMiddleware("s3cret", true, okHandler())
	if code := do(h, "/mcp/manifest", ""); code != http.StatusUnauthorized {
		t.Errorf("/mcp/manifest no bearer (guardManifest): want 401, got %d", code)
	}
	if code := do(h, "/mcp/manifest", "s3cret"); code != http.StatusOK {
		t.Errorf("/mcp/manifest right bearer (guardManifest): want 200, got %d", code)
	}
}
