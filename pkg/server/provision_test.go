package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// fakePoder stands in for a worker's tunnel: it answers the create request the
// way a pode does.
func fakePoder(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "container-1", "ip": "10.0.0.9"})
	}))
}

// gate is the registration check: without the platform token a peer cannot ask
// this instance to deliver anything.
func TestProvision_RequiresThePlatformToken(t *testing.T) {
	memory := store.NewMemoryStores()
	tunnelStore := tunnel.NewTunnelStore()
	dirStore := tunnel.NewDirectTunnelStore()
	mux := http.NewServeMux()
	mountProvision(mux, Config{Token: "shared", NodeURL: "http://self"}, tunnelStore, memory.Sandboxes, memory.Jobs)

	body := `{"sandbox":{"name":"x"},"poder_id":"p1"}`
	for _, tok := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodPost, internalProvisionPath, strings.NewReader(body))
		if tok != "" {
			req.Header.Set(internalProvisionHeader, tok)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("token %q: %d, want 403", tok, rec.Code)
		}
	}
	_ = dirStore
}

// A peer that believes we hold the tunnel, when we do not, gets a 409 — the
// caller needs to know the difference between "delivered" and "this node has no
// such worker" so it can fail the record instead of hanging.
func TestProvision_UnknownTunnelIsConflict(t *testing.T) {
	memory := store.NewMemoryStores()
	tunnelStore := tunnel.NewTunnelStore()
	mux := http.NewServeMux()
	mountProvision(mux, Config{Token: "shared"}, tunnelStore, memory.Sandboxes, memory.Jobs)

	req := httptest.NewRequest(http.MethodPost, internalProvisionPath,
		strings.NewReader(`{"sandbox":{"name":"x"},"poder_id":"nope"}`))
	req.Header.Set(internalProvisionHeader, "shared")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

// wantsPeerDelivery is the routing decision, and it must never point at
// ourselves: that would be a forwarding loop between one instance and itself.
func TestWantsPeerDelivery(t *testing.T) {
	memory := store.NewMemoryStores()
	owners := memory.TunnelOwners
	if err := owners.Claim("p1", "http://peer"); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Token: "t", NodeURL: "http://self"}
	if peer, ok := wantsPeerDelivery(cfg, owners, "p1"); !ok || peer != "http://peer" {
		t.Fatalf("got %q %v, want http://peer", peer, ok)
	}
	// The node that owns the tunnel is us: deliver locally.
	if _, ok := wantsPeerDelivery(Config{Token: "t", NodeURL: "http://peer"}, owners, "p1"); ok {
		t.Fatal("decided to forward to ourselves")
	}
	// Single instance: no forwarding.
	if _, ok := wantsPeerDelivery(Config{Token: "t"}, owners, "p1"); ok {
		t.Fatal("forwarded without a node URL")
	}
	// An expired claim has no owner.
	if _, ok := wantsPeerDelivery(cfg, owners, "never-claimed"); ok {
		t.Fatal("forwarded for a worker nobody owns")
	}
}

var _ = podpkg.JobStatusPending
