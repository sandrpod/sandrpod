// Copyright 2026 SandrPod Contributors
package relay

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortTempDir returns a directory with a path short enough for a unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "relay-")
	if err != nil {
		t.Skipf("no short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// A connection to the relay comes out the other side of the unix socket.
func TestServe_ForwardsToUnixSocket(t *testing.T) {
	// Short path on purpose: a unix socket path must fit in sun_path (104 bytes
	// on macOS, 108 on Linux), and a t.TempDir() path under /var/folders is
	// already most of that budget. The real feature has the same constraint.
	sock := filepath.Join(shortTempDir(t), "upstream.sock")
	upstream, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer upstream.Close()
	go func() {
		for {
			c, err := upstream.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c) // echo
			}(c)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Reserve a port, then let Serve take it over.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	go func() { _ = Serve(ctx, addr, sock, "") }()

	var conn net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err = net.Dial("tcp", addr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay never accepted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if line != "ping\n" {
		t.Fatalf("relay carried %q", line)
	}
}

// A missing socket must not wedge the listener: the next connection is still
// accepted (and fails on its own).
func TestServe_MissingSocketDoesNotWedge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	go func() { _ = Serve(ctx, addr, filepath.Join(shortTempDir(t), "nope.sock"), "") }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay never accepted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
