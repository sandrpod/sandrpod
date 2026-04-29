// Copyright 2026 SandrPod
// IPC round-trip tests.

package permission

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// echoNotifier returns a fixed canned response and records what it received.
// Used to verify the wire encoding round-trips correctly.
type echoNotifier struct {
	gotRequests []Request
	resp        PromptResponse
}

func (e *echoNotifier) Ask(ctx context.Context, req Request) (PromptResponse, error) {
	e.gotRequests = append(e.gotRequests, req)
	return e.resp, nil
}

func TestIPC_RoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "authz.sock")
	echo := &echoNotifier{resp: PromptAllowPermanent}

	srv := NewIPCServer(sock, echo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer srv.Stop()

	// Give the server a moment to begin accepting.
	time.Sleep(50 * time.Millisecond)

	cli := NewIPCClient(sock)
	resp, err := cli.Ask(context.Background(), Request{
		Path:      "/Users/test/foo",
		Mode:      ModeReadWrite,
		Caller:    "test",
		SessionID: "sess-xyz",
		Reason:    "unit test",
	})
	if err != nil {
		t.Fatalf("client Ask: %v", err)
	}
	if resp != PromptAllowPermanent {
		t.Errorf("response: got %q, want %q", resp, PromptAllowPermanent)
	}
	if len(echo.gotRequests) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(echo.gotRequests))
	}
	got := echo.gotRequests[0]
	if got.Path != "/Users/test/foo" || got.Mode != ModeReadWrite ||
		got.SessionID != "sess-xyz" || got.Caller != "test" {
		t.Errorf("server received wrong fields: %+v", got)
	}
}

func TestIPC_FallbackWhenSocketMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.sock")
	fallbackCalled := 0
	fallback := &stubNotifier{resp: PromptAllowOnce}
	cli := &IPCClient{
		SocketPath: missing,
		FallbackOnUnavailable: notifierFunc(func(ctx context.Context, req Request) (PromptResponse, error) {
			fallbackCalled++
			return fallback.Ask(ctx, req)
		}),
	}

	resp, err := cli.Ask(context.Background(), Request{Path: "/x", Mode: ModeRead})
	if err != nil {
		t.Fatalf("Ask should succeed via fallback: %v", err)
	}
	if resp != PromptAllowOnce {
		t.Errorf("want PromptAllowOnce from fallback, got %q", resp)
	}
	if fallbackCalled != 1 {
		t.Errorf("fallback called %d times, want 1", fallbackCalled)
	}
}

func TestIPC_DenyOnUnreachableWithoutFallback(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.sock")
	cli := NewIPCClient(missing)
	resp, err := cli.Ask(context.Background(), Request{Path: "/x", Mode: ModeRead})
	if err == nil {
		t.Error("expected an error when tray unreachable and no fallback")
	}
	if resp != PromptDeny {
		t.Errorf("want PromptDeny, got %q", resp)
	}
}

// notifierFunc adapts a function into a Notifier (test convenience only).
type notifierFunc func(context.Context, Request) (PromptResponse, error)

func (f notifierFunc) Ask(ctx context.Context, req Request) (PromptResponse, error) {
	return f(ctx, req)
}
