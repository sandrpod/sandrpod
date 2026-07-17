// Copyright 2026 SandrPod Contributors
// Toolbox - code execution service
// Runs inside sandbox containers

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sandrpod/sandrpod/pkg/mcpbridge"
	"github.com/sandrpod/sandrpod/pkg/toolbox"
)

var (
	port  = flag.Int("port", 8080, "Toolbox server port")
	token = flag.String("token", os.Getenv("TOOLBOX_TOKEN"), "Bearer token for authentication (empty = no auth)")
	help  = flag.Bool("help", false, "Show help")

	// MCP bridge: aggregates stdio MCP servers from mcp.json into a single
	// /mcp endpoint. Enabled by default so an in-sandbox agent can register
	// new MCP servers at runtime by editing mcp.json (hot-reload picks them
	// up). The bridge starts cleanly even when mcp.json is absent.
	mcpEnabled       = flag.Bool("mcp-enabled", envBool("SANDRPOD_MCP_ENABLED", true), "Enable the MCP bridge at /mcp")
	mcpConfig        = flag.String("mcp-config", os.Getenv("SANDRPOD_MCP_CONFIG"), "Path to mcp.json (default: OS config dir / mcp.json)")
	mcpHotReload     = flag.Bool("mcp-hot-reload", envBool("SANDRPOD_MCP_HOT_RELOAD", true), "Watch mcp.json and diff-reload on change")
	mcpToken         = flag.String("mcp-token", os.Getenv("SANDRPOD_MCP_TOKEN"), "Shared secret required on /mcp requests (empty = no MCP auth)")
	mcpGuardManifest = flag.Bool("mcp-guard-manifest", envBool("SANDRPOD_MCP_GUARD_MANIFEST", false), "Also require -mcp-token on /mcp/manifest (default false: manifest is read-only metadata, reachable with platform auth alone)")
)

func envBool(key string, def bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes":
		return true
	case "0", "false", "FALSE", "no":
		return false
	default:
		return def
	}
}

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	log.Printf("Starting SandrPod Toolbox v0.2.0 on port %d", *port)

	addr := fmt.Sprintf(":%d", *port)
	server := toolbox.NewServer(addr, *token)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Optional MCP bridge. Mounted at /mcp on the toolbox server.
	mgr := installMCPBridge(ctx, server)

	// Start the server
	go func() {
		if err := server.Start(); err != nil {
			log.Printf("Toolbox server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down Toolbox...")
	// Drain in-flight MCP tool calls before tearing down the bridge children.
	if mgr != nil {
		drainCtx, dc := context.WithTimeout(context.Background(), 10*time.Second)
		if err := mgr.Shutdown(drainCtx, 10*time.Second); err != nil {
			log.Printf("MCP bridge: drain incomplete: %v", err)
		}
		dc()
	}
	shutdownCtx, c := context.WithTimeout(context.Background(), 10*toolbox.CleanupTimeout)
	defer c()
	server.Stop(shutdownCtx)
}

// installMCPBridge starts the MCP bridge (if enabled) and mounts its handler
// on the toolbox server at /mcp. Returns the manager for graceful shutdown, or
// nil when disabled / failed to start (the toolbox keeps serving without MCP).
func installMCPBridge(ctx context.Context, server *toolbox.Server) *mcpbridge.ChildManager {
	if !*mcpEnabled {
		log.Printf("MCP bridge: disabled (-mcp-enabled=false)")
		return nil
	}

	path := *mcpConfig
	if path == "" {
		path = mcpbridge.DefaultConfigPath()
	}

	// Permission/Audit are agent (employee-PC) concerns and are intentionally
	// nil here: a server-side container has no interactive user, and the
	// container itself is the security boundary. The bridge substitutes an
	// allow-all gate when Permission is nil.
	// OAuth: enabled so auth=oauth entries work here too, tokens beside the
	// config. Note the loopback callback is only browser-reachable on a real
	// host — inside a container the flow can't complete, so OAuth entries are
	// an agent-first feature (docs/MCP_AUTH.md); the entry parks in
	// waiting_auth with the URL visible on the admin surface.
	mgr := mcpbridge.NewManager(mcpbridge.ManagerOptions{
		ConfigPath: path,
		HotReload:  *mcpHotReload,
		Logger:     log.Default(),
		OAuth: &mcpbridge.OAuthOptions{
			TokenDir: filepath.Join(filepath.Dir(path), "oauth"),
		},
	})

	if err := mgr.Start(ctx); err != nil {
		log.Printf("MCP bridge: start failed (%s): %v — bridge disabled", path, err)
		return nil
	}

	snap := mgr.Snapshot()
	if len(snap) == 0 {
		log.Printf("MCP bridge ready: config=%s no servers yet (hot_reload=%v will pick up a later mcp.json create)", path, *mcpHotReload)
	} else {
		log.Printf("MCP bridge ready: config=%s servers=%d hot_reload=%v", path, len(snap), *mcpHotReload)
		for _, s := range snap {
			log.Printf("  %-20s state=%-10s tools=%d", s.Name, s.State, s.ToolCount)
		}
	}

	var handler http.Handler = mcpbridge.NewHTTPHandler(mgr)
	if *mcpToken != "" {
		handler = mcpbridge.TokenMiddleware(*mcpToken, *mcpGuardManifest, handler)
		log.Printf("MCP bridge: shared-secret auth enabled (token length=%d, guard_manifest=%v)", len(*mcpToken), *mcpGuardManifest)
	} else {
		log.Printf("MCP bridge: WARNING — no -mcp-token set; any caller that reaches /mcp can invoke tools")
	}
	server.SetMCPHandler(handler)
	return mgr
}
