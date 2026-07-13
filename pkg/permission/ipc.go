// Copyright 2026 SandrPod
// IPC protocol between sandrpod-agent (daemon) and sandrpod-tray (user-session GUI).
//
// Why a separate process at all?
//
//   On macOS the LaunchDaemon vs LaunchAgent split is hard: a daemon launched
//   at boot has no user session and CAN NOT show a GUI dialog (osascript will
//   silently fail or block). Conversely, a process that opens GUI windows
//   should not be the one holding a long-lived TCP tunnel — it dies with the
//   user session, restarts on login, and would tear down sandbox connections.
//
//   So sandrpod-agent runs as a LaunchDaemon (system context, always-on,
//   tunnel owner) and sandrpod-tray runs as a LaunchAgent (user session,
//   GUI owner). They communicate over a Unix socket whose path lives in the
//   user's home directory — meaning each macOS user has their own pair, and
//   the file-permission of 0600 prevents another local user from spoofing
//   prompts.
//
// Protocol
//
//   Wire format: one JSON object per line, UTF-8.
//
//   Request:
//     {"op": "ask",
//      "path": "/Users/x/Documents/foo.xlsx",
//      "mode": "r",
//      "caller": "files.read",
//      "session_id": "orch_42_abc",
//      "reason": "summarize sales report"}
//
//   Response:
//     {"response": "allow_permanent"}      // or allow_once / allow_session / deny / timeout
//   or:
//     {"error": "no_user_session"}         // tray not running, etc.
//
// Versioning
//
//   We do NOT version the wire format yet — both binaries ship from the same
//   git revision. If we later need wire-level evolution, add a "v" field on
//   the request and let the tray reject unknown versions.

package permission

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sandrpod/sandrpod/pkg/homedir"
)

// DefaultSocketPath returns the canonical Unix socket path for the running
// user. Living in $HOME (rather than /tmp or /var/run) gives us natural
// per-user isolation and avoids /tmp's mode-tickle attacks.
func DefaultSocketPath() (string, error) {
	return filepath.Join(homedir.DataDir(), "authz.sock"), nil
}

// ipcRequest is the wire form of a permission ask.
type ipcRequest struct {
	Op        string `json:"op"`
	Path      string `json:"path"`
	Mode      Mode   `json:"mode"`
	Caller    string `json:"caller,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ipcResponse is the wire form of a tray's reply.
type ipcResponse struct {
	Response PromptResponse `json:"response,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// IPCClient is a Notifier that delegates Ask() to a tray running on the local
// Unix socket. Each Ask opens a fresh connection — the tray is not on the hot
// path of code execution (only path-permission misses), so per-call connection
// overhead is negligible and lets the tray restart freely.
type IPCClient struct {
	SocketPath string

	// FallbackOnUnavailable, if non-nil, is called when the tray socket cannot
	// be reached (file missing, ECONNREFUSED, etc.). Use this to fall back to
	// an in-process prompter so a missing tray doesn't fail-close every request.
	// If nil, IPCClient returns PromptDeny on tray unavailability.
	FallbackOnUnavailable Notifier
}

// NewIPCClient constructs an IPCClient with sensible defaults.
func NewIPCClient(socketPath string) *IPCClient {
	return &IPCClient{SocketPath: socketPath}
}

// Ask implements Notifier by sending a single JSON-line over the socket and
// reading exactly one JSON-line response.
func (c *IPCClient) Ask(ctx context.Context, req Request) (PromptResponse, error) {
	conn, err := dialUnixWithCtx(ctx, c.SocketPath)
	if err != nil {
		if c.FallbackOnUnavailable != nil {
			return c.FallbackOnUnavailable.Ask(ctx, req)
		}
		// Fail-close: the human had no chance to consent, so deny.
		return PromptDeny, fmt.Errorf("tray not reachable at %s: %w", c.SocketPath, err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	wire := ipcRequest{
		Op:        "ask",
		Path:      req.Path,
		Mode:      req.Mode,
		Caller:    req.Caller,
		SessionID: req.SessionID,
		Reason:    req.Reason,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return PromptDeny, fmt.Errorf("marshal ipc request: %w", err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return PromptDeny, fmt.Errorf("write ipc request: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return PromptDeny, fmt.Errorf("read ipc response: %w", err)
		}
		return PromptDeny, errors.New("ipc connection closed before response")
	}
	var resp ipcResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return PromptDeny, fmt.Errorf("parse ipc response: %w", err)
	}
	if resp.Error != "" {
		return PromptDeny, fmt.Errorf("tray error: %s", resp.Error)
	}
	if resp.Response == "" {
		return PromptDeny, errors.New("tray returned empty response")
	}
	return resp.Response, nil
}

// IPCServer accepts connections from sandrpod-agent and forwards each ask to
// the wrapped Notifier (typically MacPrompter inside the tray binary).
type IPCServer struct {
	socketPath string
	notifier   Notifier

	mu     sync.Mutex
	lis    net.Listener
	closed bool
}

// NewIPCServer prepares (but does not yet bind) an IPC server.
//
// notifier is the user-facing prompter that actually renders the dialog.
// Inside cmd/sandrpod-tray that's a notify.MacPrompter (osascript). Tests
// can pass a stub.
func NewIPCServer(socketPath string, notifier Notifier) *IPCServer {
	return &IPCServer{socketPath: socketPath, notifier: notifier}
}

// Start binds the Unix socket and begins serving in a goroutine. It is
// idempotent only across separate IPCServer instances; calling Start twice on
// the same instance returns an error.
//
// If a stale socket file from a previous crash exists at socketPath, Start
// will unlink it. We do NOT chmod the socket file directory — caller (the
// tray binary) is responsible for ensuring the parent dir is 0700.
func (s *IPCServer) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.lis != nil {
		s.mu.Unlock()
		return errors.New("ipc server already started")
	}

	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0700); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("mkdir socket parent: %w", err)
	}
	// Best-effort cleanup: stale socket from a previous run.
	_ = os.Remove(s.socketPath)

	lis, err := net.Listen("unix", s.socketPath)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("listen unix %s: %w", s.socketPath, err)
	}
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		_ = lis.Close()
		s.mu.Unlock()
		return fmt.Errorf("chmod 0600 socket: %w", err)
	}
	s.lis = lis
	s.mu.Unlock()

	go s.serveLoop(ctx)

	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	return nil
}

// Stop closes the listener and removes the socket file. Safe to call multiple
// times; only the first call has an effect.
func (s *IPCServer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.lis != nil {
		_ = s.lis.Close()
	}
	_ = os.Remove(s.socketPath)
}

func (s *IPCServer) serveLoop(ctx context.Context) {
	for {
		conn, err := s.lis.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			// Transient accept error — pause briefly to avoid a hot loop.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *IPCServer) handleConn(parent context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	if !scanner.Scan() {
		return
	}
	var req ipcRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		writeIPCResp(conn, ipcResponse{Error: "invalid_json: " + err.Error()})
		return
	}
	if req.Op != "ask" {
		writeIPCResp(conn, ipcResponse{Error: fmt.Sprintf("unknown op %q", req.Op)})
		return
	}

	// Per-request timeout aligned with manager.DefaultPromptDeadline so
	// the dialog can't hold the agent forever even if the tray is buggy.
	ctx, cancel := context.WithTimeout(parent, DefaultPromptDeadline)
	defer cancel()

	resp, err := s.notifier.Ask(ctx, Request{
		Path:      req.Path,
		Mode:      req.Mode,
		Caller:    req.Caller,
		SessionID: req.SessionID,
		Reason:    req.Reason,
	})
	if err != nil {
		writeIPCResp(conn, ipcResponse{Error: err.Error()})
		return
	}
	writeIPCResp(conn, ipcResponse{Response: resp})
}

func writeIPCResp(conn net.Conn, r ipcResponse) {
	data, _ := json.Marshal(r)
	_, _ = conn.Write(append(data, '\n'))
}

// dialUnixWithCtx dials a unix socket honoring ctx for both connect timeout
// and abort. net.Dialer's DialContext is the canonical way; we just constrain
// the connect timeout to 1 second since locally-connected Unix sockets either
// answer instantly or aren't there.
func dialUnixWithCtx(ctx context.Context, path string) (net.Conn, error) {
	d := net.Dialer{Timeout: time.Second}
	return d.DialContext(ctx, "unix", path)
}
