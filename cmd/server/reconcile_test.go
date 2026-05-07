// Copyright 2024 SandrPod

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// dialTunnel starts a fake Poder HTTP server (handler) wrapped inside a real
// yamux tunnel and returns the client-side PoderTunnel. The layout mirrors the
// real architecture:
//
//	API Server (yamux client / PoderTunnel.Client) ←→ Poder worker (yamux server)
//
// reconcilePoderSandboxes uses t.Client, which dials yamux streams to the
// server side — so the fake handler is actually exercised.
func dialTunnel(t *testing.T, handler http.Handler) *tunnel.PoderTunnel {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	ready := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("ws upgrade: %v", err)
			return
		}
		// Poder side: yamux server, serves HTTP over mux streams.
		session, err := yamux.Server(tunnel.NewWSConn(ws), yamux.DefaultConfig())
		if err != nil {
			t.Logf("yamux.Server: %v", err)
			return
		}
		close(ready)
		httpSrv := &http.Server{Handler: handler}
		httpSrv.Serve(session) //nolint:errcheck
	}))

	wsURL := "ws" + ts.URL[len("http"):]
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// API Server side: yamux client, wrapped in PoderTunnel.
	clientTunnel, err := tunnel.NewPoderTunnel("clt", clientWS)
	if err != nil {
		t.Fatalf("client tunnel: %v", err)
	}

	<-ready

	t.Cleanup(func() {
		clientTunnel.Close()
		ts.Close()
	})
	return clientTunnel
}

// sandboxMux returns an http.Handler simulating Poder's GET /sandboxes/{name}.
// Names in alive → 200; others → 404.
func sandboxMux(alive map[string]bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/sandboxes/"):]
		if alive[name] {
			w.WriteHeader(http.StatusOK)
		} else {
			http.NotFound(w, r)
		}
	})
	return mux
}

func addSandbox(store *podpkg.SandboxStore, name, poderID string, state podpkg.State) {
	_ = store.Add(&podpkg.SandboxInfo{
		Name:    name,
		PoderID: poderID,
		State:   state,
	})
}

func TestReconcilePoderSandboxes_MarksGoneAsError(t *testing.T) {
	tun := dialTunnel(t, sandboxMux(map[string]bool{"sb-alive": true}))

	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-alive", "fake-poder", podpkg.StateRunning)
	addSandbox(store, "sb-gone", "fake-poder", podpkg.StateRunning)

	reconcilePoderSandboxes("fake-poder", tun, store)

	if sb, _ := store.Get("sb-alive"); sb.State != podpkg.StateRunning {
		t.Errorf("sb-alive: expected RUNNING, got %s", sb.State)
	}
	if sb, _ := store.Get("sb-gone"); sb.State != podpkg.StateError {
		t.Errorf("sb-gone: expected ERROR, got %s", sb.State)
	}
}

func TestReconcilePoderSandboxes_SkipsNonRunning(t *testing.T) {
	tun := dialTunnel(t, sandboxMux(map[string]bool{}))

	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-stopped", "fake-poder", podpkg.StateStopped)

	reconcilePoderSandboxes("fake-poder", tun, store)

	if sb, _ := store.Get("sb-stopped"); sb.State != podpkg.StateStopped {
		t.Errorf("sb-stopped: expected STOPPED unchanged, got %s", sb.State)
	}
}

func TestReconcilePoderSandboxes_AllAlive(t *testing.T) {
	tun := dialTunnel(t, sandboxMux(map[string]bool{"sb-a": true, "sb-b": true}))

	store := podpkg.NewSandboxStore()
	addSandbox(store, "sb-a", "fake-poder", podpkg.StateRunning)
	addSandbox(store, "sb-b", "fake-poder", podpkg.StateRunning)

	reconcilePoderSandboxes("fake-poder", tun, store)

	for _, name := range []string{"sb-a", "sb-b"} {
		if sb, _ := store.Get(name); sb.State != podpkg.StateRunning {
			t.Errorf("%s: expected RUNNING, got %s", name, sb.State)
		}
	}
}
