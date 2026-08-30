package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// storeWith returns a sandbox repo holding one sandbox in the given state, and
// a tunnel store that already has its poder's tunnel — so the only thing left
// to decide the outcome is the sandbox's state.
func storeWith(name string, state podpkg.State) (podpkg.SandboxRepository, *tunnel.TunnelStore) {
	ss := store.NewMemoryStores().Sandboxes
	_ = ss.Add(&podpkg.SandboxInfo{Name: name, PoderID: "P", State: state})
	ts := tunnel.NewTunnelStore()
	ts.Set("P", nil) // presence is what sandboxTunnel checks; it is never dialled here
	return ss, ts
}

// Requests bound for the toolbox INSIDE the sandbox used to be proxied into a
// stopped container, where they did not fail — they hung, returning nothing at
// all until the caller gave up (measured: 60s, no response). The state was
// already in hand and thrown away.
func TestSandboxTunnel_RefusesWhenNotRunning(t *testing.T) {
	for _, state := range []podpkg.State{
		podpkg.StateStopped, podpkg.StatePending, podpkg.StateStarting,
		podpkg.StateStopping, podpkg.StateError, podpkg.StateTerminated,
	} {
		ss, ts := storeWith("sb", state)
		rec := httptest.NewRecorder()
		_, _, ok := sandboxTunnel("sb", httptest.NewRequest("POST", "/x", nil), ss,
			ts, tunnel.NewDirectTunnelStore(), stubOwners{}, "", rec)

		if ok {
			t.Errorf("state %s: allowed through to a container that is not up", state)
		}
		if rec.Code != http.StatusConflict {
			t.Errorf("state %s: answered %d, want 409", state, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), string(state)) {
			t.Errorf("state %s: body %q does not say which state it is in", state, rec.Body.String())
		}
	}
}

func TestSandboxTunnel_AllowsRunning(t *testing.T) {
	ss, ts := storeWith("sb", podpkg.StateRunning)
	rec := httptest.NewRecorder()
	if _, _, ok := sandboxTunnel("sb", httptest.NewRequest("POST", "/x", nil), ss,
		ts, tunnel.NewDirectTunnelStore(), stubOwners{}, "", rec); !ok {
		t.Fatalf("a RUNNING sandbox was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// logs, start, stop and snapshot talk to the poder ABOUT a sandbox and are
// exactly what you reach for when it is stopped. They must not inherit the
// guard.
func TestSandboxPoderTunnel_AllowsStopped(t *testing.T) {
	ss, ts := storeWith("sb", podpkg.StateStopped)
	rec := httptest.NewRecorder()
	if _, _, ok := sandboxPoderTunnel("sb", httptest.NewRequest("GET", "/x", nil), ss,
		ts, tunnel.NewDirectTunnelStore(), stubOwners{}, "", rec); !ok {
		t.Fatalf("poder-level access to a STOPPED sandbox was refused: %d %s",
			rec.Code, rec.Body.String())
	}
}
