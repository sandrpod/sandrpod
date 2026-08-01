// Copyright 2026 SandrPod Contributors

package tunnel

import (
	"io"
	"net/http"
	"testing"
)

// TestStreamClientReusesStreams counts yamux streams, because that is the
// resource this leaks and nothing else shows it — a leaked stream is invisible
// to lsof and to the file table, which are the two signals that surfaced the
// same bug in the tray (#26).
//
// The mechanism: a cleanly-finished response is read to EOF, so Close() returns
// the connection to the Transport's idle pool instead of closing it. If the
// Transport is a per-call temporary, that pooled stream is never closed and
// never collected — the connection's read/write goroutines keep it reachable.
// One yamux stream per successful streaming request, held for the life of the
// tunnel, on both ends.
func TestStreamClientReusesStreams(t *testing.T) {
	tun, cleanup := newTunnelPair(t, "x", okHandler())
	defer cleanup()

	// Same *http.Client every time — the singleton is the fix, so assert it
	// directly rather than only through its effect.
	if a, b := tun.StreamClient(), tun.StreamClient(); a != b {
		t.Error("StreamClient returns a new client per call; it must be one per tunnel")
	}

	before := tun.session.NumStreams()
	const calls = 15
	for i := 0; i < calls; i++ {
		resp, err := tun.StreamClient().Get("http://x/")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		// Read to EOF, then close: exactly what flushCopy does for a stream
		// that finishes normally, and precisely the path that leaks.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	grew := tun.session.NumStreams() - before
	// Keep-alive reuse means the pool serves them all. A couple of spare
	// streams is fine; one per call is the bug.
	if grew > 3 {
		t.Errorf("%d streaming requests left %d yamux streams open — the pool is not being reused",
			calls, grew)
	}
}

// TestStreamClientNoOverallTimeout keeps the property that made this a separate
// client in the first place: streaming responses must not be cut off by a
// client-wide deadline.
func TestStreamClientNoOverallTimeout(t *testing.T) {
	tun, cleanup := newTunnelPair(t, "x", okHandler())
	defer cleanup()
	if got := tun.StreamClient().Timeout; got != 0 {
		t.Errorf("StreamClient timeout = %v, want 0", got)
	}
	var _ *http.Client = tun.StreamClient()
}
