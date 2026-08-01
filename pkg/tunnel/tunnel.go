// Package tunnel provides a reverse WebSocket+yamux tunnel between
// API Server and Poder nodes. Poder dials in; API Server multiplexes
// HTTP requests back through the same connection.
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// wsConn wraps a gorilla WebSocket connection as io.ReadWriteCloser for yamux.
// gorilla allows one concurrent reader and one concurrent writer, so writes
// are protected by a mutex while reads are expected from a single goroutine.
type wsConn struct {
	conn   *websocket.Conn
	reader io.Reader
	wmu    sync.Mutex
}

func newWSConn(conn *websocket.Conn) *wsConn {
	return &wsConn{conn: conn}
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader != nil {
			n, err := c.reader.Read(p)
			if err == io.EOF {
				c.reader = nil
				continue
			}
			return n, err
		}
		_, r, err := c.conn.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = r
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error {
	return c.conn.Close()
}

// WriteText sends a text frame on the WebSocket. Used by Poder for heartbeat
// messages. Thread-safe.
func (c *wsConn) WriteText(msg []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

// PoderTunnel represents an active yamux tunnel to a Poder node.
// The Poder node initiates the WebSocket connection; the API Server
// acts as the yamux client and opens streams to Poder's HTTP server.
type PoderTunnel struct {
	ID       string
	session  *yamux.Session
	Client   *http.Client      // routes HTTP through yamux streams
	WSDialer *websocket.Dialer // routes WebSocket through yamux streams (PTY)

	// Built on first use rather than in the constructor: most tunnels never
	// carry a streaming request, and this keeps one client per tunnel instead
	// of one per request. See StreamClient.
	streamOnce sync.Once
	streamCli  *http.Client
}

// NewPoderTunnel creates a tunnel from an already-upgraded WebSocket connection.
// The caller (API Server) becomes the yamux client; Poder serves HTTP over yamux.
func NewPoderTunnel(id string, ws *websocket.Conn) (*PoderTunnel, error) {
	return NewPoderTunnelFromConn(id, newWSConn(ws))
}

// NewPoderTunnelFromConn is the transport-agnostic constructor used by
// NewPoderTunnel and by tests that want to wire the two halves over a
// net.Pipe instead of a real WebSocket. Production callers should use
// NewPoderTunnel; this is exported to give tests a clean injection point.
func NewPoderTunnelFromConn(id string, conn io.ReadWriteCloser) (*PoderTunnel, error) {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 5 * time.Second // faster dead-connection detection (was 30s)
	cfg.ConnectionWriteTimeout = 5 * time.Second
	session, err := yamux.Client(conn, cfg)
	if err != nil {
		return nil, fmt.Errorf("yamux client: %w", err)
	}

	// openStream opens a yamux stream, aborting early if ctx is cancelled.
	// session.Open() itself does not accept a context, so we run it in a
	// goroutine and race it against ctx.Done().
	openStream := func(ctx context.Context) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			conn, err := session.Open()
			ch <- result{conn, err}
		}()
		select {
		case r := <-ch:
			return r.conn, r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return openStream(ctx)
			},
			MaxIdleConnsPerHost: 32,
		},
		Timeout: 60 * time.Second,
	}

	wsDialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return openStream(context.Background())
		},
		HandshakeTimeout: 10 * time.Second,
	}

	return &PoderTunnel{
		ID:       id,
		session:  session,
		Client:   client,
		WSDialer: wsDialer,
	}, nil
}

// Closed reports whether the yamux session has been closed.
func (t *PoderTunnel) Closed() bool {
	return t.session.IsClosed()
}

// Wait blocks until the yamux session is detected as closed.
// It uses periodic Ping to actively probe the connection; Ping returns
// immediately with an error when the session is already dead, so the
// worst-case detection latency equals the ping interval (~3s).
func (t *PoderTunnel) Wait() {
	for {
		if t.session.IsClosed() {
			return
		}
		if _, err := t.session.Ping(); err != nil {
			return
		}
		time.Sleep(3 * time.Second)
	}
}

// Close shuts down the yamux session and underlying WebSocket.
// Close tears down the tunnel. Closing the yamux session already takes every
// stream with it; dropping the idle pools first is belt and braces, and keeps
// the intent legible next to StreamClient's comment about what pooling costs.
func (t *PoderTunnel) Close() error {
	closeIdle(t.Client)
	closeIdle(t.streamCli)
	return t.session.Close()
}

func closeIdle(c *http.Client) {
	if c == nil {
		return
	}
	if tr, ok := c.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// TunnelStore is a thread-safe map of active tunnels keyed by an arbitrary string.
// Used for Poder tunnels (keyed by Poder ID) and for direct sandbox agent tunnels
// (keyed by sandbox name).
type TunnelStore struct {
	mu      sync.RWMutex
	tunnels map[string]*PoderTunnel
}

func NewTunnelStore() *TunnelStore {
	return &TunnelStore{tunnels: make(map[string]*PoderTunnel)}
}

// NewDirectTunnelStore creates a TunnelStore for direct sandbox agent tunnels.
// Semantically distinct from Poder tunnels: keyed by sandbox name, not Poder ID.
func NewDirectTunnelStore() *TunnelStore {
	return &TunnelStore{tunnels: make(map[string]*PoderTunnel)}
}

func (s *TunnelStore) Add(t *PoderTunnel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tunnels[t.ID] = t
}

// Set stores a tunnel under an explicit key (used by direct sandbox tunnels,
// where the key is the sandbox name rather than t.ID).
func (s *TunnelStore) Set(key string, t *PoderTunnel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tunnels[key] = t
}

func (s *TunnelStore) Get(id string) (*PoderTunnel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tunnels[id]
	return t, ok
}

// Keys returns the ids of all tunnels currently held on this instance. Used by
// the multi-instance ownership refresher to keep tunnel_owners.updated_at fresh.
func (s *TunnelStore) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.tunnels))
	for k := range s.tunnels {
		keys = append(keys, k)
	}
	return keys
}

func (s *TunnelStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tunnels, id)
}

// StreamClient returns the tunnel's streaming HTTP client: no overall timeout,
// so SSE and other long-lived responses are not cut off mid-flight.
//
// One client per tunnel, not one per call. An http.Transport owns a connection
// pool, and a discarded Transport does not close the idle connections left in
// it — the connection's read/write goroutines keep it reachable, so the garbage
// collector never gets there. A response that finishes cleanly is read to EOF,
// which returns its stream to that pool rather than closing it; build the
// Transport per request and every successful streaming request strands a yamux
// stream at both ends for the life of the tunnel. Invisible to lsof, unlike the
// same mistake in the tray.
func (t *PoderTunnel) StreamClient() *http.Client {
	t.streamOnce.Do(func() {
		t.streamCli = &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return t.session.Open()
				},
				// Bound the pool and expire idle streams, so a poder that goes
				// away does not leave this side holding dead ones.
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 0, // streaming: no overall deadline
		}
	})
	return t.streamCli
}

// NewWSConn exposes the wsConn constructor for use by Poder (cmd/poder).
func NewWSConn(conn *websocket.Conn) io.ReadWriteCloser {
	return newWSConn(conn)
}
