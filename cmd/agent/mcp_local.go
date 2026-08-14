package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/sandrpod/sandrpod/pkg/homedir"
	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

// defaultMCPLocalSocketPath sits beside mcp.sock and authz.sock, under the
// same per-user directory and the same convention.
func defaultMCPLocalSocketPath() string {
	return filepath.Join(homedir.DataDir(), "mcp-local.sock")
}

// startMCPLocalServer serves the MCP protocol endpoint (/mcp) on an AF_UNIX
// socket in addition to the tunnel, so a host on this same machine reaches
// the bridge directly instead of leaving the machine and coming back.
//
// The toolbox listens only on the yamux tunnel, which is right for code
// execution — the control plane should be the only way in. But an MCP Apps
// host does two things that involve no model, no orchestration and no
// server-side state: it fetches a tool's interface HTML with resources/read,
// and it relays a tools/call the user triggered by clicking inside that
// interface. Routing a click through the WAN twice to reach a process on the
// same machine costs about a second; over loopback it is about a millisecond.
//
// This shares the manager with the tunnel entry point, so the alias
// namespace (alias__tool, ui://alias/path), the permission gate and the
// audit trail are identical. The local path is a shortcut, not a bypass.
//
// The auth boundary matches the admin socket: file permissions (0600) on
// POSIX, parent-directory ACL on Windows 1803+. Deliberately no
// --mcp-token — that guards a network port reachable by anyone who can
// route to it, whereas a caller here is already this user, who can read
// everything under ~/.sandrpod/ including the token itself.
//
// Best-effort throughout: a failure to bind logs and returns, leaving the
// tunnel path serving as before.
func startMCPLocalServer(ctx context.Context, mgr *mcpbridge.ChildManager) {
	sockPath := defaultMCPLocalSocketPath()

	// Clear a stale socket, or net.Listen reports "address already in use"
	// after an agent that died without cleaning up. On Windows the file is
	// not reported as a socket, so fall back to removing whatever is at the
	// canonical path — safe because the parent directory is per-user.
	if fi, err := os.Stat(sockPath); err == nil {
		if fi.Mode()&os.ModeSocket != 0 || runtime.GOOS == "windows" {
			_ = os.Remove(sockPath)
		} else {
			log.Printf("MCP local: %s exists and is not a socket — refusing to clobber", sockPath)
			return
		}
	}

	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		log.Printf("MCP local: mkdir %s failed: %v — local socket disabled", filepath.Dir(sockPath), err)
		return
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		// Windows below build 17134 has no AF_UNIX. Same handling as the
		// admin socket: log and carry on; callers fall back to the tunnel.
		log.Printf("MCP local: listen %s failed: %v — local socket disabled", sockPath, err)
		return
	}
	tightenSocketPerms(sockPath)

	mux := http.NewServeMux()
	h := mcpbridge.NewHTTPHandler(mgr)
	mux.Handle("/mcp", h)
	mux.Handle("/mcp/", h)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Printf("MCP local socket: %s", sockPath)
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("MCP local serve: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = os.Remove(sockPath)
	}()
}
