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

// defaultMCPAdminSocketPath returns the conventional path for the local
// management socket the tray dials. Mirrors the permission authz socket
// convention (~/.sandrpod/<name>.sock).
func defaultMCPAdminSocketPath() string {
	return filepath.Join(homedir.DataDir(), "mcp.sock")
}

// startMCPAdminServer binds an AF_UNIX HTTP server that exposes the
// mcpbridge admin endpoints (manifest, reload, restart, disable). Local
// only: on POSIX the socket file permissions are the auth boundary; on
// Windows ≥10 1803 the parent-directory ACL is. Best-effort — failure
// to bind never aborts the agent (the bridge keeps serving /mcp).
//
// AF_UNIX support on Windows requires build 17134+; older Windows hosts
// simply log "listen failed" and continue without the admin channel.
func startMCPAdminServer(ctx context.Context, mgr *mcpbridge.ChildManager) {
	sockPath := defaultMCPAdminSocketPath()

	// If a stale socket file exists, remove it. Otherwise net.Listen
	// returns "address already in use" even if the previous agent died.
	// On Windows the file appears as a regular file rather than a socket
	// (ModeSocket isn't set), so we fall back to removing any pre-existing
	// file at the canonical path — safe because the parent dir is per-user.
	if fi, err := os.Stat(sockPath); err == nil {
		if fi.Mode()&os.ModeSocket != 0 || runtime.GOOS == "windows" {
			_ = os.Remove(sockPath)
		} else {
			log.Printf("MCP admin: %s exists and is not a socket — refusing to clobber", sockPath)
			return
		}
	}

	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		log.Printf("MCP admin: mkdir %s failed: %v — admin socket disabled", filepath.Dir(sockPath), err)
		return
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Printf("MCP admin: listen %s failed: %v — admin socket disabled", sockPath, err)
		return
	}
	tightenSocketPerms(sockPath)

	srv := &http.Server{
		Handler:           mcpbridge.NewAdminHandler(mgr),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("MCP admin socket: %s", sockPath)
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("MCP admin serve: %v", err)
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
