// Copyright 2026 SandrPod Contributors
// The in-sandbox relay: TCP on loopback → unix socket on the host.
//
// A sandbox with its own network namespace has loopback and nothing else, so
// the egress proxy — a host process listening on a unix socket — is reachable
// only through a file. Proxy-aware tools speak TCP to 127.0.0.1, so something
// inside the namespace has to bridge the two, and that something is this: a few
// dozen lines that accept on loopback, connect to the socket, and copy bytes
// both ways.
//
// It ships inside the sandbox as the agent binary itself (`sandrpod-agent
// relay …`, bind-mounted read-only), so the OS sandbox needs no second binary
// on the host and the relay is versioned with the proxy it talks to.
//
// It carries no policy and makes no decisions: every byte goes to the proxy,
// which decides. A sandbox that talks to the relay is exactly as free as the
// proxy allows, which is the point.
package relay

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// Serve accepts TCP connections on listen and forwards each to the unix socket
// at socketPath until ctx is cancelled or the listener fails.
//
// readyPath, when set, is created once the listener is up. The sandbox wrapper
// waits for it before starting the command: without that, a command can lose
// the race against the relay and fail to connect to a proxy that is about to
// exist, which from inside looks exactly like a policy denial.
func Serve(ctx context.Context, listen, socketPath, readyPath string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("relay: listen %s: %w", listen, err)
	}
	defer ln.Close()
	if readyPath != "" {
		if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
			return fmt.Errorf("relay: ready file %s: %w", readyPath, err)
		}
		defer os.Remove(readyPath)
	}

	// Closing the listener unblocks Accept when the context ends.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("relay: accept: %w", err)
			}
		}
		go forward(conn, socketPath)
	}
}

func forward(client net.Conn, socketPath string) {
	defer client.Close()

	upstream, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		// The proxy is gone or the mount is wrong. Say so per attempt rather
		// than dropping the connection silently: from inside the sandbox,
		// "the relay cannot reach the proxy" and "policy denied me" would
		// otherwise look identical.
		log.Printf("relay: dial %s: %v", socketPath, err)
		return
	}
	defer upstream.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, client); closeWrite(upstream) }()
	go func() { defer wg.Done(); _, _ = io.Copy(client, upstream); closeWrite(client) }()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
