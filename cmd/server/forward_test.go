package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

// TestForwardToNode_WebSocket proves the inter-node forward transparently
// proxies a WebSocket (the PTY-shell path): a client dialing the "receiving"
// node reaches the echo backend standing in for the tunnel-owning node.
func TestForwardToNode_WebSocket(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, msg)
		}
	}))
	defer echo.Close()

	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardToNode(echo.URL, w, r) // receiving node forwards to the owner
	}))
	defer fwd.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(fwd.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial through forwarder: %v", err)
	}
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.ReadMessage()
	if err != nil || string(msg) != "ping" {
		t.Fatalf("WS not echoed through forward: msg=%q err=%v", msg, err)
	}
}

// TestShouldPersistActivity_Throttles locks in that the hot-path activity write
// fires at most once per interval per sandbox.
func TestShouldPersistActivity_Throttles(t *testing.T) {
	now := time.Now()
	name := "sb-throttle"
	if !shouldPersistActivity(name, now) {
		t.Fatal("first call must persist")
	}
	if shouldPersistActivity(name, now.Add(time.Second)) {
		t.Error("within interval must skip")
	}
	if !shouldPersistActivity(name, now.Add(activityPersistEvery+time.Second)) {
		t.Error("after interval must persist")
	}
}
