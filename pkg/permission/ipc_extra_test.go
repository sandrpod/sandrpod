// Copyright 2026 SandrPod
// Additional IPC tests: server error paths, double-start, stop idempotency,
// notifier-error propagation, and the tray-error response branch.

package permission

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortSock returns a socket path short enough to satisfy the ~104-byte
// sockaddr_un.sun_path limit on macOS/BSD. t.TempDir() combined with long test
// names easily overflows it, so we mkdir a tiny dir under os.TempDir().
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "a.sock")
}

// dialAndSend opens the socket, writes a raw line, and returns the first
// response line. Used to exercise server-side error branches directly.
func dialAndSend(t *testing.T, sock, line string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("no response: %v", sc.Err())
	}
	return sc.Text()
}

func startServer(t *testing.T, n Notifier) (string, *IPCServer) {
	t.Helper()
	sock := shortSock(t)
	srv := NewIPCServer(sock, n)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server start: %v", err)
	}
	t.Cleanup(srv.Stop)
	time.Sleep(30 * time.Millisecond) // let Accept loop come up
	return sock, srv
}

func TestIPCServer_InvalidJSON(t *testing.T) {
	sock, _ := startServer(t, &echoNotifier{resp: PromptAllowOnce})
	resp := dialAndSend(t, sock, "{not json")
	if !contains(resp, "invalid_json") {
		t.Errorf("expected invalid_json error, got %q", resp)
	}
}

func TestIPCServer_UnknownOp(t *testing.T) {
	sock, _ := startServer(t, &echoNotifier{resp: PromptAllowOnce})
	resp := dialAndSend(t, sock, `{"op":"frobnicate","path":"/x"}`)
	if !contains(resp, "unknown op") {
		t.Errorf("expected unknown-op error, got %q", resp)
	}
}

func TestIPCServer_NotifierError_PropagatesToClient(t *testing.T) {
	errNotifier := notifierFunc(func(context.Context, Request) (PromptResponse, error) {
		return PromptDeny, errors.New("dialog backend exploded")
	})
	sock, _ := startServer(t, errNotifier)

	cli := NewIPCClient(sock)
	resp, err := cli.Ask(context.Background(), Request{Path: "/x", Mode: ModeRead})
	if err == nil {
		t.Error("client should surface the tray error")
	}
	if resp != PromptDeny {
		t.Errorf("tray error should map to PromptDeny, got %q", resp)
	}
}

func TestIPCClient_TrayReturnsErrorField(t *testing.T) {
	// Notifier that triggers the server's error-response path.
	sock, _ := startServer(t, notifierFunc(func(context.Context, Request) (PromptResponse, error) {
		return "", errors.New("no_user_session")
	}))
	cli := NewIPCClient(sock)
	_, err := cli.Ask(context.Background(), Request{Path: "/x", Mode: ModeRead})
	if err == nil || !contains(err.Error(), "no_user_session") {
		t.Errorf("expected tray error to propagate, got %v", err)
	}
}

func TestIPCServer_DoubleStart(t *testing.T) {
	sock := shortSock(t)
	srv := NewIPCServer(sock, &echoNotifier{resp: PromptAllowOnce})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer srv.Stop()
	if err := srv.Start(ctx); err == nil {
		t.Error("second Start on the same instance must error")
	}
}

func TestIPCServer_StopIdempotent(t *testing.T) {
	sock := shortSock(t)
	srv := NewIPCServer(sock, &echoNotifier{resp: PromptAllowOnce})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	srv.Stop()
	srv.Stop() // must not panic
}

func TestIPCServer_ContextCancelStopsServer(t *testing.T) {
	sock := shortSock(t)
	srv := NewIPCServer(sock, &echoNotifier{resp: PromptAllowOnce})
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	cancel() // ctx cancellation should drive Stop()
	time.Sleep(50 * time.Millisecond)

	// Socket should no longer accept connections.
	if _, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
		t.Error("expected dial to fail after context cancellation")
	}
}

func TestIPCClient_ContextAlreadyCanceled(t *testing.T) {
	sock, _ := startServer(t, &echoNotifier{resp: PromptAllowOnce})
	cli := NewIPCClient(sock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dial should fail immediately
	resp, err := cli.Ask(ctx, Request{Path: "/x", Mode: ModeRead})
	if err == nil {
		t.Error("expected error when context is already canceled")
	}
	if resp != PromptDeny {
		t.Errorf("want PromptDeny on dial failure, got %q", resp)
	}
}

func TestNewIPCServer_StaleSocketReplaced(t *testing.T) {
	sock := shortSock(t)
	// Simulate a leftover socket file from a crashed previous run: a plain file
	// sitting at the socket path. net.Listen("unix", …) fails with EADDRINUSE
	// unless Start unlinks it first, so this genuinely exercises the
	// stale-socket reclamation path (Start's best-effort os.Remove).
	//
	// The previous version bound a first server and closed its listener to
	// "leave a stale file", but Go's UnixListener unlinks on Close (so no file
	// was left), and cancelling the first server's context triggered its async
	// Stop → os.Remove(sock), which raced the second server's bind and could
	// unlink the live socket. Creating the file directly is both correct and
	// deterministic.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := NewIPCServer(sock, &echoNotifier{resp: PromptAllowOnce})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start over stale socket: %v", err)
	}
	defer srv.Stop()

	// No sleep needed: Start binds+listens synchronously, so a connect queues
	// in the listen backlog until serveLoop accepts it.
	cli := NewIPCClient(sock)
	if _, err := cli.Ask(context.Background(), Request{Path: "/x", Mode: ModeRead}); err != nil {
		t.Errorf("server over reclaimed stale socket should respond: %v", err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
