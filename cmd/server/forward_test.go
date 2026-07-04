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

// TestForwardE2B_ForwardsToOwnerNode: the E2B envd/code hook reverse-proxies to
// the peer node owning the sandbox's tunnel (multi-instance), carrying the
// loop-guard header — the E2B surface must route like the native sandboxTunnel.
func TestForwardE2B_ForwardsToOwnerNode(t *testing.T) {
	var gotGuard, gotHost string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGuard = r.Header.Get(forwardedHeader)
		gotHost = r.Host
		w.WriteHeader(288)
	}))
	defer peer.Close()

	d := e2bDeps{
		cfg:         serverConfig{NodeURL: "http://self:8080"},
		sandboxes:   newSandbox("sb"),
		tunnelStore: tunnel.NewTunnelStore(),
		directStore: tunnel.NewDirectTunnelStore(),
		owners:      stubOwners{node: peer.URL, ok: true},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/execute", nil)
	// The E2B gateway on the owner node routes by Host; the forward must carry
	// the original vanity host through unchanged, not the peer's own host.
	req.Host = "49999-sb.e2e.local"

	if handled := d.forwardE2B(rec, req, "sb"); !handled {
		t.Fatal("expected forwardE2B to handle (forward), got false")
	}
	if rec.Code != 288 {
		t.Fatalf("not forwarded to peer: code=%d", rec.Code)
	}
	if gotGuard != "1" {
		t.Errorf("peer missing loop-guard header, got %q", gotGuard)
	}
	if gotHost != "49999-sb.e2e.local" {
		t.Errorf("peer received rewritten Host %q, want the E2B vanity host preserved", gotHost)
	}
}

// TestForwardE2B_ServesLocally covers the paths that must NOT forward: single
// instance, an already-forwarded request (loop guard), an unknown owner, and a
// self-owned sandbox. Each must fall through to local serving (false).
func TestForwardE2B_ServesLocally(t *testing.T) {
	base := func() e2bDeps {
		return e2bDeps{
			cfg:         serverConfig{NodeURL: "http://self:8080"},
			sandboxes:   newSandbox("sb"),
			tunnelStore: tunnel.NewTunnelStore(),
			directStore: tunnel.NewDirectTunnelStore(),
			owners:      stubOwners{node: "http://peer", ok: true},
		}
	}
	req := func() *http.Request { return httptest.NewRequest("GET", "/files", nil) }

	t.Run("single instance", func(t *testing.T) {
		d := base()
		d.cfg.NodeURL = ""
		if d.forwardE2B(httptest.NewRecorder(), req(), "sb") {
			t.Error("single instance must not forward")
		}
	})
	t.Run("already forwarded", func(t *testing.T) {
		d := base()
		r := req()
		r.Header.Set(forwardedHeader, "1")
		if d.forwardE2B(httptest.NewRecorder(), r, "sb") {
			t.Error("already-forwarded request must not forward again")
		}
	})
	t.Run("no peer owner", func(t *testing.T) {
		d := base()
		d.owners = stubOwners{ok: false}
		if d.forwardE2B(httptest.NewRecorder(), req(), "sb") {
			t.Error("unknown owner must serve locally")
		}
	})
	t.Run("owner is self", func(t *testing.T) {
		d := base()
		d.owners = stubOwners{node: "http://self:8080", ok: true}
		if d.forwardE2B(httptest.NewRecorder(), req(), "sb") {
			t.Error("self-owned must not forward to self")
		}
	})
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
