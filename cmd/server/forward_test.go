package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// stubOwners is a TunnelOwnerRepository returning a fixed owner node.
type stubOwners struct {
	node string
	ok   bool
}

func (s stubOwners) Claim(string, string) error    { return nil }
func (s stubOwners) Release(string, string) error  { return nil }
func (s stubOwners) NodeFor(string) (string, bool) { return s.node, s.ok }

func newSandbox(name string) podpkg.SandboxRepository {
	ss := store.NewMemoryStores().Sandboxes
	_ = ss.Add(&podpkg.SandboxInfo{Name: name, PoderID: "P", State: podpkg.StateRunning})
	return ss
}

// TestSandboxTunnel_ForwardsToOwnerNode: the tunnel isn't local, the owner map
// points at a peer, so the request is reverse-proxied there (carrying the
// loop-guard header) and sandboxTunnel reports handled (ok=false).
func TestSandboxTunnel_ForwardsToOwnerNode(t *testing.T) {
	var gotGuard string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGuard = r.Header.Get(forwardedHeader)
		w.WriteHeader(299)
		_, _ = w.Write([]byte("served-by-peer"))
	}))
	defer peer.Close()

	ss := newSandbox("sb")
	req := httptest.NewRequest("POST", "/api/v1/sandboxes/execute?sandbox=sb", nil)
	rec := httptest.NewRecorder()

	_, _, ok := sandboxTunnel("sb", req, ss,
		tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore(),
		stubOwners{node: peer.URL, ok: true}, "http://self:8080", rec)

	if ok {
		t.Fatal("expected ok=false (request forwarded), got true")
	}
	if rec.Code != 299 || rec.Body.String() != "served-by-peer" {
		t.Fatalf("not forwarded to peer: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if gotGuard != "1" {
		t.Errorf("peer did not receive loop-guard header, got %q", gotGuard)
	}
}

// TestSandboxTunnel_LoopGuard: a request already forwarded once is not forwarded
// again (a stale owner map degrades to a clean 503, not a loop).
func TestSandboxTunnel_LoopGuard(t *testing.T) {
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set(forwardedHeader, "1")
	rec := httptest.NewRecorder()

	_, _, ok := sandboxTunnel("sb", req, newSandbox("sb"),
		tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore(),
		stubOwners{node: "http://peer", ok: true}, "http://self", rec)

	if ok || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("loop guard failed: ok=%v code=%d", ok, rec.Code)
	}
}

// TestSandboxTunnel_NoForwardToSelf: the owner map names THIS node but the
// tunnel is gone locally → 503, never forward to self.
func TestSandboxTunnel_NoForwardToSelf(t *testing.T) {
	req := httptest.NewRequest("POST", "/x", nil)
	rec := httptest.NewRecorder()

	_, _, ok := sandboxTunnel("sb", req, newSandbox("sb"),
		tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore(),
		stubOwners{node: "http://self", ok: true}, "http://self", rec)

	if ok || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("self-forward guard failed: ok=%v code=%d", ok, rec.Code)
	}
}

// TestSandboxTunnel_SingleInstanceNoForward: with no node URL configured
// (single instance), a missing local tunnel is a plain 503 — no owner lookup.
func TestSandboxTunnel_SingleInstanceNoForward(t *testing.T) {
	req := httptest.NewRequest("POST", "/x", nil)
	rec := httptest.NewRecorder()

	_, _, ok := sandboxTunnel("sb", req, newSandbox("sb"),
		tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore(),
		stubOwners{node: "http://peer", ok: true}, "", rec) // nodeURL="" → single instance

	if ok || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("single-instance path wrong: ok=%v code=%d", ok, rec.Code)
	}
}
