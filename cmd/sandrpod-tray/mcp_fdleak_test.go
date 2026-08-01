// Copyright 2026 SandrPod Contributors

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestAdminClientReusesConnections counts descriptors, because that is the only
// thing that actually proves this. An http.Transport owns a connection pool and
// a discarded Transport never closes the idle connections left in it — the GC
// will not do it. Building one per call leaked a socket per call at both ends,
// and the tray polls every ten seconds: enough to exhaust the system file table
// in under a week and take unrelated software (Docker, in the report) with it.
// leakPerCall reproduces the old shape — a fresh Transport per call — so the
// test can be shown to fail against it rather than merely passing against the fix.
var leakPerCall = os.Getenv("LEAK_PER_CALL") != ""

func TestAdminClientReusesConnections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	// Not t.TempDir(): its path blows past the 104-byte sun_path limit on macOS
	// and Listen fails with a bare "invalid argument".
	dir, err := os.MkdirTemp("/tmp", "fdleak")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")

	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	// Count what the server side is holding open — the client's leak shows up
	// there as accepted connections that are never released.
	accepted := make(chan struct{}, 512)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	}), ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			accepted <- struct{}{}
		}
	}}
	go srv.Serve(lis)
	defer srv.Close()

	cli := &mcpAdminClient{sockPath: sock, hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConnsPerHost: 1,
	}}}

	const calls = 20
	for i := 0; i < calls; i++ {
		if leakPerCall {
			cli = &mcpAdminClient{sockPath: sock, hc: &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			}}}
		}
		var out map[string]any
		if err := cli.get("/admin/manifest", &out); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	// Keep-alive reuse means one connection for all of them. Allow a couple for
	// scheduling slack; what must not happen is one per call.
	n := len(accepted)
	if n > 3 {
		t.Errorf("%d calls opened %d connections — the pool is not being reused", calls, n)
	}
	_ = os.Remove(sock)
}
