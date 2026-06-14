package toolbox

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The toolbox server mounts an optional MCP bridge handler at /mcp so that
// standalone (poder/dedicated container) deployments get the same MCP surface
// the agent exposes. These tests pin the mounting contract.

func TestHandler_MountsMCPHandlerWhenSet(t *testing.T) {
	s := NewServer("", "")
	hit := make(chan string, 1)
	s.SetMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))

	h := s.Handler()

	for _, path := range []string{"/mcp", "/mcp/manifest"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got status %d, want 200", path, rec.Code)
		}
		select {
		case got := <-hit:
			if got != path {
				t.Fatalf("MCP handler saw %q, want %q", got, path)
			}
		default:
			t.Fatalf("%s did not reach the MCP handler", path)
		}
	}
}

func TestHandler_NoMCPHandlerLeavesMCPUnrouted(t *testing.T) {
	s := NewServer("", "")
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	// With no MCP handler installed, /mcp is not a registered route → 404.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unset MCP handler: got status %d, want 404", rec.Code)
	}
}
