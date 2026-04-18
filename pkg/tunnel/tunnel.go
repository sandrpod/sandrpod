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
}

// NewPoderTunnel creates a tunnel from an already-upgraded WebSocket connection.
// The caller (API Server) becomes the yamux client; Poder serves HTTP over yamux.
func NewPoderTunnel(id string, ws *websocket.Conn) (*PoderTunnel, error) {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 30 * time.Second
	session, err := yamux.Client(newWSConn(ws), cfg)
	if err != nil {
		return nil, fmt.Errorf("yamux client: %w", err)
	}

	openStream := func() (net.Conn, error) {
		return session.Open()
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return openStream()
			},
			MaxIdleConnsPerHost: 32,
		},
		Timeout: 60 * time.Second,
	}

	wsDialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return openStream()
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

// Close shuts down the yamux session and underlying WebSocket.
func (t *PoderTunnel) Close() error {
	return t.session.Close()
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

func (s *TunnelStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tunnels, id)
}

// StreamClient returns an HTTP client with no timeout, suitable for
// Server-Sent Events and other long-lived streaming responses.
func (t *PoderTunnel) StreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return t.session.Open()
			},
		},
		Timeout: 0,
	}
}

// NewWSConn exposes the wsConn constructor for use by Poder (cmd/poder).
func NewWSConn(conn *websocket.Conn) io.ReadWriteCloser {
	return newWSConn(conn)
}
