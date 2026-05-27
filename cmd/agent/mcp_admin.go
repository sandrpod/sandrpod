package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
)

// defaultMCPAdminSocketPath returns the conventional path for the local
// management socket the tray dials. Mirrors the permission authz socket
// convention (~/.sandrpod/<name>.sock).
func defaultMCPAdminSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "mcp.sock"
	}
	return filepath.Join(home, ".sandrpod", "mcp.sock")
}

// startMCPAdminServer binds a Unix-socket HTTP server that exposes the
// mcpbridge admin endpoints (manifest, reload, restart, disable). Local
// only (Unix-socket file permissions are the auth boundary). Best-effort:
// failure to bind never aborts the agent.
//
// On Windows the function is a no-op — the tray on Windows can call
// /mcp/manifest directly via the existing yamux tunnel and doesn't need
// the admin socket for MVP. (Named-pipe support can come later.)
func startMCPAdminServer(ctx context.Context, mgr *mcpbridge.ChildManager) {
	sockPath := defaultMCPAdminSocketPath()

	// If a stale socket file exists, remove it. Otherwise net.Listen
	// returns "address already in use" even if the previous agent died.
	if fi, err := os.Stat(sockPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(sockPath)
	} else if err == nil {
		log.Printf("MCP admin: %s exists and is not a socket — refusing to clobber", sockPath)
		return
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
	// 0600 so only the same uid can connect.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		log.Printf("MCP admin: chmod %s failed: %v (continuing — may be world-accessible)", sockPath, err)
	}

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
