package tunnel

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// serveYamuxHTTP wires the server half of a yamux session onto an
// http.Server with the given handler. It is the mirror image of the
// yamux *client* that NewPoderTunnelFromConn builds: the Poder side serves
// HTTP, the API-server side (the tunnel) dials streams against it.
//
// Returns a cleanup func the caller should defer.
func serveYamuxHTTP(t *testing.T, conn io.ReadWriteCloser, handler http.Handler) func() {
	t.Helper()
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 5 * time.Second
	cfg.ConnectionWriteTimeout = 5 * time.Second
	session, err := yamux.Server(conn, cfg)
	if err != nil {
		t.Fatalf("yamux.Server: %v", err)
	}
	srv := &http.Server{Handler: handler}
	done := make(chan struct{})
	go func() {
		// Serve returns once the session (acting as a net.Listener) is closed.
		_ = srv.Serve(session)
		close(done)
	}()
	return func() {
		_ = session.Close()
		<-done
	}
}

// newTunnelPair returns a connected (tunnel-client, server-cleanup) pair over
// an in-memory net.Pipe. The server side answers every request with handler.
func newTunnelPair(t *testing.T, id string, handler http.Handler) (*PoderTunnel, func()) {
	t.Helper()
	c1, c2 := net.Pipe()
	tun, err := NewPoderTunnelFromConn(id, c1)
	if err != nil {
		_ = c1.Close()
		_ = c2.Close()
		t.Fatalf("NewPoderTunnelFromConn: %v", err)
	}
	stop := serveYamuxHTTP(t, c2, handler)
	cleanup := func() {
		_ = tun.Close()
		stop()
	}
	return tun, cleanup
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

// --- PoderTunnel HTTP round trip ----------------------------------------

func TestPoderTunnel_SingleHTTPRoundTrip(t *testing.T) {
	tun, cleanup := newTunnelPair(t, "x", okHandler())
	defer cleanup()

	resp, err := tun.Client.Get("http://x/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", string(body), "ok")
	}
}

func TestPoderTunnel_EchoesRequestPath(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.URL.Path)
	})
	tun, cleanup := newTunnelPair(t, "x", handler)
	defer cleanup()

	resp, err := tun.Client.Get("http://x/hello/world")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "/hello/world" {
		t.Fatalf("echoed path = %q, want %q", string(body), "/hello/world")
	}
}

func TestPoderTunnel_ConcurrentRequests(t *testing.T) {
	// Each request echoes back a per-request token from the query string,
	// so we can assert that multiplexed streams do not cross-talk.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.URL.Query().Get("id"))
	})
	tun, cleanup := newTunnelPair(t, "x", handler)
	defer cleanup()

	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := fmt.Sprintf("http://x/?id=%d", i)
			resp, err := tun.Client.Get(url)
			if err != nil {
				errs <- fmt.Errorf("req %d: %w", i, err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- fmt.Errorf("req %d read: %w", i, err)
				return
			}
			want := fmt.Sprintf("%d", i)
			if string(body) != want {
				errs <- fmt.Errorf("req %d: body = %q, want %q", i, string(body), want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPoderTunnel_StreamClientRoundTrip(t *testing.T) {
	tun, cleanup := newTunnelPair(t, "x", okHandler())
	defer cleanup()

	client := tun.StreamClient()
	if client.Timeout != 0 {
		t.Fatalf("StreamClient timeout = %v, want 0", client.Timeout)
	}
	resp, err := client.Get("http://x/")
	if err != nil {
		t.Fatalf("StreamClient Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", string(body), "ok")
	}
}

func TestPoderTunnel_IDIsStored(t *testing.T) {
	tun, cleanup := newTunnelPair(t, "poder-42", okHandler())
	defer cleanup()
	if tun.ID != "poder-42" {
		t.Fatalf("ID = %q, want %q", tun.ID, "poder-42")
	}
	if tun.Client == nil || tun.WSDialer == nil {
		t.Fatalf("Client/WSDialer must be non-nil")
	}
}

// --- Closed / Wait / Close lifecycle ------------------------------------

func TestPoderTunnel_ClosedAfterClose(t *testing.T) {
	tun, cleanup := newTunnelPair(t, "x", okHandler())
	defer cleanup()

	if tun.Closed() {
		t.Fatalf("Closed() = true before Close()")
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// IsClosed should report true essentially immediately after Close.
	deadline := time.Now().Add(2 * time.Second)
	for !tun.Closed() {
		if time.Now().After(deadline) {
			t.Fatalf("Closed() never became true after Close()")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPoderTunnel_ClosedAfterConnClose(t *testing.T) {
	c1, c2 := net.Pipe()
	tun, err := NewPoderTunnelFromConn("x", c1)
	if err != nil {
		t.Fatalf("NewPoderTunnelFromConn: %v", err)
	}
	stop := serveYamuxHTTP(t, c2, okHandler())
	defer stop()

	if tun.Closed() {
		t.Fatalf("Closed() = true on a fresh tunnel")
	}

	// Closing the underlying pipe end the tunnel reads from should bring the
	// yamux session down.
	_ = c1.Close()

	deadline := time.Now().Add(2 * time.Second)
	for !tun.Closed() {
		if time.Now().After(deadline) {
			t.Fatalf("Closed() never became true after underlying conn closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPoderTunnel_WaitReturnsAfterClose(t *testing.T) {
	tun, cleanup := newTunnelPair(t, "x", okHandler())
	defer cleanup()

	returned := make(chan struct{})
	go func() {
		tun.Wait()
		close(returned)
	}()

	// Wait should still be blocking while the session is alive.
	select {
	case <-returned:
		t.Fatalf("Wait() returned while session was still open")
	case <-time.After(100 * time.Millisecond):
	}

	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-returned:
		// Wait observed the closed session via IsClosed() or a failed Ping.
	case <-time.After(5 * time.Second):
		t.Fatalf("Wait() did not return after Close()")
	}
}

func TestPoderTunnel_WaitReturnsWhenConnDrops(t *testing.T) {
	c1, c2 := net.Pipe()
	tun, err := NewPoderTunnelFromConn("x", c1)
	if err != nil {
		t.Fatalf("NewPoderTunnelFromConn: %v", err)
	}
	stop := serveYamuxHTTP(t, c2, okHandler())
	defer stop()

	returned := make(chan struct{})
	go func() {
		tun.Wait()
		close(returned)
	}()

	// Drop the transport: Wait()'s Ping should fail and it should return.
	_ = c1.Close()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatalf("Wait() did not return after conn drop")
	}
}

// --- TunnelStore --------------------------------------------------------

func TestTunnelStore_AddGetRemove(t *testing.T) {
	s := NewTunnelStore()
	if _, ok := s.Get("missing"); ok {
		t.Fatalf("Get on empty store returned ok=true")
	}

	t1 := &PoderTunnel{ID: "a"}
	s.Add(t1)

	got, ok := s.Get("a")
	if !ok {
		t.Fatalf("Get(a) ok=false after Add")
	}
	if got != t1 {
		t.Fatalf("Get(a) returned a different pointer")
	}

	s.Remove("a")
	if _, ok := s.Get("a"); ok {
		t.Fatalf("Get(a) ok=true after Remove")
	}
}

func TestTunnelStore_SetUsesExplicitKey(t *testing.T) {
	s := NewDirectTunnelStore()
	// Tunnel ID differs from the storage key, mirroring direct sandbox tunnels
	// keyed by sandbox name rather than t.ID.
	tun := &PoderTunnel{ID: "internal-id"}
	s.Set("sandbox-name", tun)

	if _, ok := s.Get("internal-id"); ok {
		t.Fatalf("Get(internal-id) should miss; Set used explicit key")
	}
	got, ok := s.Get("sandbox-name")
	if !ok || got != tun {
		t.Fatalf("Get(sandbox-name) = (%v, %v), want the stored tunnel", got, ok)
	}
}

func TestTunnelStore_SetOverwrites(t *testing.T) {
	s := NewTunnelStore()
	first := &PoderTunnel{ID: "1"}
	second := &PoderTunnel{ID: "2"}
	s.Set("k", first)
	s.Set("k", second)
	got, ok := s.Get("k")
	if !ok || got != second {
		t.Fatalf("Set did not overwrite: got %v", got)
	}
}

func TestTunnelStore_RemoveMissingIsNoop(t *testing.T) {
	s := NewTunnelStore()
	s.Remove("nope") // must not panic
}

func TestTunnelStore_ConcurrentAccess(t *testing.T) {
	s := NewTunnelStore()
	const workers = 50
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k-%d", i%8)
			tun := &PoderTunnel{ID: key}
			for j := 0; j < 100; j++ {
				s.Add(tun)
				s.Set(key, tun)
				_, _ = s.Get(key)
				s.Remove(key)
			}
		}(i)
	}
	wg.Wait()
}

// --- wsConn over a real WebSocket ---------------------------------------

// TestWSConn_ReadWriteClose exercises newWSConn / NewWSConn against a real
// gorilla WebSocket server: a binary frame written on one side is read back
// on the other, then Close tears the connection down.
func TestWSConn_ReadWriteClose(t *testing.T) {
	upgrader := websocket.Upgrader{}
	// The server echoes binary frames straight back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	rwc := NewWSConn(clientConn) // exported constructor
	payload := []byte("hello-yamux-frame")

	n, err := rwc.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write n = %d, want %d", n, len(payload))
	}

	buf := make([]byte, len(payload))
	got, err := io.ReadFull(rwc, buf)
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf[:got]) != string(payload) {
		t.Fatalf("read back %q, want %q", string(buf[:got]), string(payload))
	}

	if err := rwc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A read after Close must surface an error rather than hanging.
	if _, err := rwc.Read(buf); err == nil {
		t.Fatalf("Read after Close returned nil error")
	}
}

// TestWSConn_WriteText covers the text-frame heartbeat path. The concrete
// type is needed for WriteText since it is not part of io.ReadWriteCloser.
func TestWSConn_WriteText(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		mt, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.TextMessage {
			received <- string(data)
		}
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()

	c := newWSConn(clientConn)
	if err := c.WriteText([]byte("ping")); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	select {
	case got := <-received:
		if got != "ping" {
			t.Fatalf("server received %q, want %q", got, "ping")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not receive text frame")
	}
}

// TestWSConn_EndToEndYamux is the integration capstone: a yamux client is
// driven over a *real* WebSocket (via NewPoderTunnel) instead of net.Pipe,
// proving the wsConn adapter satisfies yamux's framing requirements.
func TestWSConn_EndToEndYamux(t *testing.T) {
	upgrader := websocket.Upgrader{}
	serverReady := make(chan func())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// The Poder side serves HTTP over a yamux *server* session.
		stop := serveYamuxHTTP(t, NewWSConn(c), okHandler())
		serverReady <- stop
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	tun, err := NewPoderTunnel("x", clientConn)
	if err != nil {
		t.Fatalf("NewPoderTunnel: %v", err)
	}
	stop := <-serverReady
	defer func() {
		_ = tun.Close()
		stop()
	}()

	resp, err := tun.Client.Get("http://x/")
	if err != nil {
		t.Fatalf("Get over real WebSocket: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("got status=%d body=%q, want 200/ok", resp.StatusCode, string(body))
	}
}
