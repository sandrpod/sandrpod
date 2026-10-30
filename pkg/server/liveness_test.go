package server

import (
	"context"
	"net"
	"testing"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// A worker whose heartbeat channel is broken but whose tunnel is up is alive. The
// heartbeat is a second connection that can fail on its own — a local proxy, a reset
// path — and marking that worker OFFLINE hides a running machine from placement and
// from its operator.
func TestCleanupOfflinePoders_TunnelOutranksHeartbeat(t *testing.T) {
	stores := store.NewMemoryStores()
	ts := tunnel.NewTunnelStore()
	const id = "p-tunneled"
	_, _ = stores.Poders.Register(&podpkg.RegisterPoderRequest{ID: id, ProviderType: "local"})
	// Stale heartbeat: well past the timeout.
	_ = stores.Poders.UpdateUsage(id, func(u *podpkg.PoderUsage) {})
	markStale(t, stores.Poders, id)

	// With a tunnel registered the sweep must leave it ONLINE.
	ts.Set(id, fakeTunnel(t, id))
	sweepOnce(stores, ts, 30*time.Second)
	if p, _ := stores.Poders.Get(id); p.State != podpkg.PoderStateOnline {
		t.Fatalf("state = %s, want ONLINE (tunnel is up)", p.State)
	}
}

// Without a tunnel the timeout still applies: that is what it is for.
func TestCleanupOfflinePoders_NoTunnelGoesOffline(t *testing.T) {
	stores := store.NewMemoryStores()
	const id = "p-orphan"
	_, _ = stores.Poders.Register(&podpkg.RegisterPoderRequest{ID: id, ProviderType: "local"})
	markStale(t, stores.Poders, id)

	sweepOnce(stores, tunnel.NewTunnelStore(), 30*time.Second)
	if p, _ := stores.Poders.Get(id); p.State != podpkg.PoderStateOffline {
		t.Fatalf("state = %s, want OFFLINE (no tunnel, no heartbeat)", p.State)
	}
}

// An OFFLINE record whose tunnel is up is brought back — the case that made a
// worker disappear from the console while its sandboxes kept running.
func TestCleanupOfflinePoders_BringsBackTunneledRecord(t *testing.T) {
	stores := store.NewMemoryStores()
	ts := tunnel.NewTunnelStore()
	const id = "p-back"
	_, _ = stores.Poders.Register(&podpkg.RegisterPoderRequest{ID: id, ProviderType: "local"})
	stores.Poders.SetOffline(id)
	ts.Set(id, fakeTunnel(t, id))

	sweepOnce(stores, ts, 30*time.Second)
	if p, _ := stores.Poders.Get(id); p.State != podpkg.PoderStateOnline {
		t.Fatalf("state = %s, want ONLINE", p.State)
	}
}

// fakeTunnel is a real tunnel object over an in-memory pipe: the liveness pass
// checks that the session is not closed, and a nil would not exercise that.
func fakeTunnel(t *testing.T, id string) *tunnel.PoderTunnel {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	tr, err := tunnel.NewPoderTunnelFromConn(id, client)
	if err != nil {
		t.Fatalf("tunnel: %v", err)
	}
	return tr
}

// markStale pushes a record's heartbeat into the past without waiting.
func markStale(t *testing.T, ps podpkg.PoderRepository, id string) {
	t.Helper()
	p, ok := ps.Get(id)
	if !ok {
		t.Fatal("record missing")
	}
	p.LastHeartbeat = time.Now().Add(-time.Hour)
}

// sweepOnce runs the sweep body once, so the test does not wait for a ticker.
func sweepOnce(stores podpkg.Stores, ts *tunnel.TunnelStore, timeout time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stop the loop immediately after the first pass
	done := make(chan struct{})
	go func() {
		cleanupOfflinePoders(ctx, stores.Poders, ts, timeout)
		close(done)
	}()
	<-done
	// The loop exits on ctx.Done() before its first tick; run the body directly to
	// keep the test deterministic.
	sweepBody(stores.Poders, ts, timeout, time.Now())
}
